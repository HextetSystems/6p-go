// Package paster provides a client for 6paster-compatible paste services.
package paster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maximumErrorBody = 8 << 10

var (
	htmlErrorElementPattern = regexp.MustCompile(`(?is)<[a-z][^>]*\bclass\s*=\s*(?:"[^"]*\berror\b[^"]*"|'[^']*\berror\b[^']*'|error)[^>]*>(.*?)</[a-z][^>]*>`)
	htmlTagPattern          = regexp.MustCompile(`(?is)<[^>]*>`)
)

// Client uploads content to a 6paster-compatible service.
type Client struct {
	server     *url.URL
	httpClient *http.Client
}

// HTTPError reports a non-redirect response from the paste service.
type HTTPError struct {
	StatusCode int
	Status     string
	Body       string
	detail     string
}

func (e *HTTPError) Error() string {
	detail := e.Body
	if e.detail != "" {
		detail = e.detail
	}
	if detail == "" {
		return fmt.Sprintf("paste service returned %s", e.Status)
	}
	return fmt.Sprintf("paste service returned %s: %s", e.Status, detail)
}

// New creates a client for serverURL. The URL may include a deployment path.
// A nil httpClient uses a copy of http.DefaultClient.
func New(serverURL string, httpClient *http.Client) (*Client, error) {
	server, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("parse server URL: %w", err)
	}
	if (server.Scheme != "http" && server.Scheme != "https") || server.Host == "" {
		return nil, errors.New("server URL must be an absolute HTTP or HTTPS URL")
	}
	if server.User != nil {
		return nil, errors.New("server URL must not contain user information")
	}
	if server.RawQuery != "" || server.Fragment != "" {
		return nil, errors.New("server URL must not contain a query or fragment")
	}
	escapedPath := strings.TrimRight(server.EscapedPath(), "/") + "/"
	server.Path, err = url.PathUnescape(escapedPath)
	if err != nil {
		return nil, fmt.Errorf("normalize server URL path: %w", err)
	}
	server.RawPath = escapedPath

	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	clientCopy := *httpClient
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	return &Client{server: server, httpClient: &clientCopy}, nil
}

// Create streams content to the paste service and returns the new paste URL.
// The TTL must be positive and resolve to a whole number of seconds.
func (c *Client) Create(ctx context.Context, content io.Reader, ttl time.Duration) (*url.URL, error) {
	if content == nil {
		return nil, errors.New("content reader is nil")
	}
	if ttl <= 0 || ttl%time.Second != 0 {
		return nil, errors.New("TTL must be a positive whole number of seconds")
	}

	var framing bytes.Buffer
	multipartWriter := multipart.NewWriter(&framing)
	if _, err := multipartWriter.CreateFormField("content"); err != nil {
		return nil, fmt.Errorf("create content form field: %w", err)
	}
	prefix := bytes.Clone(framing.Bytes())
	framing.Reset()
	if err := multipartWriter.WriteField("ttl", strconv.FormatInt(int64(ttl/time.Second), 10)); err != nil {
		return nil, fmt.Errorf("create TTL form field: %w", err)
	}
	if err := multipartWriter.Close(); err != nil {
		return nil, fmt.Errorf("finish multipart body: %w", err)
	}

	uploadComplete := make(chan struct{})
	body := io.MultiReader(
		bytes.NewReader(prefix),
		content,
		&completionReader{reader: bytes.NewReader(framing.Bytes()), done: uploadComplete},
	)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.server.String(), body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Content-Type", multipartWriter.FormDataContentType())

	response, requestErr := c.httpClient.Do(request)
	if requestErr != nil {
		return nil, fmt.Errorf("submit paste: %w", requestErr)
	}
	defer response.Body.Close()

	if response.StatusCode < 300 || response.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, maximumErrorBody+1))
		detail := htmlResponseDetail(response.Header.Get("Content-Type"), body)
		bodyText := strings.TrimSpace(string(body))
		if len(body) > maximumErrorBody {
			bodyText = strings.TrimSpace(string(body[:maximumErrorBody])) + "..."
		}
		return nil, &HTTPError{StatusCode: response.StatusCode, Status: response.Status, Body: bodyText, detail: detail}
	}
	select {
	case <-uploadComplete:
	default:
		return nil, errors.New("paste service responded before the upload completed")
	}

	location, err := response.Location()
	if err != nil {
		return nil, fmt.Errorf("paste service returned an invalid redirect: %w", err)
	}
	if (location.Scheme != "http" && location.Scheme != "https") || location.Host == "" {
		return nil, errors.New("paste service returned a non-HTTP redirect URL")
	}
	return location, nil
}

func htmlResponseDetail(contentType string, body []byte) string {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	isHTML := mediaType == "text/html" || mediaType == "application/xhtml+xml"
	if !isHTML {
		detectedType, _, _ := mime.ParseMediaType(http.DetectContentType(body))
		isHTML = detectedType == "text/html"
	}
	if !isHTML {
		return ""
	}

	match := htmlErrorElementPattern.FindSubmatch(body)
	if len(match) < 2 {
		return "HTML response did not contain a paste redirect"
	}
	text := htmlTagPattern.ReplaceAllString(string(match[1]), " ")
	text = strings.Join(strings.Fields(html.UnescapeString(text)), " ")
	if text == "" {
		return "HTML response did not contain a paste redirect"
	}
	return text
}

// completionReader closes done as soon as all of its bytes have been handed to
// the HTTP transport. It deliberately does not wait for one more Read call to
// observe EOF, because a transport may receive a response after the final bytes
// are sent and stop reading the request body at that point.
type completionReader struct {
	reader   *bytes.Reader
	done     chan struct{}
	complete bool
}

func (r *completionReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if r.reader.Len() == 0 && !r.complete {
		r.complete = true
		close(r.done)
	}
	return n, err
}
