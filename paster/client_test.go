package paster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCreateMultipartAndRedirects(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		location string
		wantPath string
	}{
		{"legacy absolute redirect", http.StatusFound, "absolute", "/p/legacy"},
		{"go relative redirect", http.StatusSeeOther, "relative", "/tools/paste/p/modern"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/tools/paste/" {
					t.Errorf("request = %s %s, want POST /tools/paste/", r.Method, r.URL.Path)
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Errorf("parse multipart form: %v", err)
				}
				if got := r.FormValue("content"); got != "hello\x00world" {
					t.Errorf("content = %q", got)
				}
				if got := r.FormValue("ttl"); got != "604800" {
					t.Errorf("ttl = %q", got)
				}
				location := "/tools/paste/p/modern"
				if tt.location == "absolute" {
					location = server.URL + "/p/legacy"
				}
				http.Redirect(w, r, location, tt.status)
			}))
			defer server.Close()

			client, err := New(server.URL+"/tools/paste", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Create(context.Background(), strings.NewReader("hello\x00world"), 7*24*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if result.Scheme != "http" || result.Host == "" || result.Path != tt.wantPath {
				t.Fatalf("result = %s", result)
			}
		})
	}
}

func TestCreateDoesNotFollowRedirect(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path == "/" {
			_, _ = io.Copy(io.Discard, r.Body)
			http.Redirect(w, r, "/p/abcdefghijklmnopqrstuvwx", http.StatusSeeOther)
			return
		}
		t.Error("client followed the paste redirect")
	}))
	defer server.Close()

	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Create(context.Background(), strings.NewReader("hello"), time.Hour); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestCreateHTTPError(t *testing.T) {
	for _, status := range []int{400, 413, 415, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "  rejected paste  \n")
			}))
			defer server.Close()
			client, err := New(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Create(context.Background(), strings.NewReader("hello"), time.Hour)
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) || httpErr.StatusCode != status || httpErr.Body != "rejected paste" {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestCreateBoundsHTTPErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, strings.Repeat("x", maximumErrorBody+100))
	}))
	defer server.Close()
	client, _ := New(server.URL, server.Client())
	_, err := client.Create(context.Background(), strings.NewReader("hello"), time.Hour)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || len(httpErr.Body) != maximumErrorBody+3 || !strings.HasSuffix(httpErr.Body, "...") {
		t.Fatalf("error = %#v, body length = %d", err, len(httpErr.Body))
	}
}

func TestCreateSummarizesHTMLResponse(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantText string
	}{
		{
			name:     "error element",
			body:     `<!doctype html><html><body><p class="notice error">Your paste &gt; the limit</p><script>irrelevant()</script></body></html>`,
			wantText: "paste service returned 200 OK: Your paste > the limit",
		},
		{
			name:     "no error element",
			body:     `<!doctype html><html><head><title>Paste form</title></head><body><form></form></body></html>`,
			wantText: "paste service returned 200 OK: HTML response did not contain a paste redirect",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/html; charset=UTF-8")
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()

			client, _ := New(server.URL, server.Client())
			_, err := client.Create(context.Background(), strings.NewReader("content"), time.Hour)
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) {
				t.Fatalf("error = %#v", err)
			}
			if err.Error() != tt.wantText {
				t.Fatalf("error text = %q, want %q", err, tt.wantText)
			}
			if httpErr.Body != tt.body {
				t.Fatalf("raw body = %q, want %q", httpErr.Body, tt.body)
			}
		})
	}
}

func TestCreateRedirectErrors(t *testing.T) {
	for _, location := range []string{"", "%gh", "mailto:user@example.com"} {
		t.Run(location, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if location != "" {
					w.Header().Set("Location", location)
				}
				w.WriteHeader(http.StatusSeeOther)
			}))
			defer server.Close()
			client, _ := New(server.URL, server.Client())
			if _, err := client.Create(context.Background(), strings.NewReader("hello"), time.Hour); err == nil {
				t.Fatal("expected redirect error")
			}
		})
	}
}

func TestCreateInputAndContextErrors(t *testing.T) {
	client, err := New("http://127.0.0.1:1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ttl := range []time.Duration{0, -time.Second, time.Second / 2} {
		if _, err := client.Create(context.Background(), strings.NewReader("x"), ttl); err == nil {
			t.Errorf("Create with TTL %s succeeded", ttl)
		}
	}
	if _, err := client.Create(context.Background(), nil, time.Second); err == nil {
		t.Error("Create with nil content succeeded")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	cancelClient, _ := New(server.URL, server.Client())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cancelClient.Create(ctx, strings.NewReader("x"), time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}

func TestCreateReaderAndTransportErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer server.Close()
	client, _ := New(server.URL, server.Client())
	sentinel := errors.New("reader failed")
	if _, err := client.Create(context.Background(), errorReader{err: sentinel}, time.Second); !errors.Is(err, sentinel) {
		t.Fatalf("reader error = %v", err)
	}

	badClient, _ := New("http://127.0.0.1:1/", &http.Client{Transport: errorTransport{err: sentinel}})
	if _, err := badClient.Create(context.Background(), strings.NewReader("x"), time.Second); !errors.Is(err, sentinel) {
		t.Fatalf("transport error = %v", err)
	}
}

func TestCreateTransportErrorDoesNotWaitForBlockedContent(t *testing.T) {
	content := &blockingReader{started: make(chan struct{}), release: make(chan struct{})}
	readDone := make(chan struct{})
	client, err := New("https://paste.example/", &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		go func() {
			_, _ = io.Copy(io.Discard, request.Body)
			close(readDone)
		}()
		<-content.started
		return nil, errors.New("transport failed")
	})})
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		_, err := client.Create(context.Background(), content, time.Second)
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "transport failed") {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(time.Second):
		close(content.release)
		<-result
		t.Fatal("Create waited for the blocked content reader after the transport failed")
	}
	close(content.release)
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("request-body reader did not stop after content was released")
	}
}

func TestCreateRejectsRedirectBeforeUploadCompletes(t *testing.T) {
	client, err := New("https://paste.example/", &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusSeeOther,
			Status:     "303 See Other",
			Header:     http.Header{"Location": []string{"https://paste.example/p/id"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.Create(context.Background(), strings.NewReader("not consumed"), time.Second); err == nil || !strings.Contains(err.Error(), "before the upload completed") {
		t.Fatalf("error = %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	for _, value := range []string{"", "example.com", "ftp://example.com", "https://user@example.com", "https://example.com/?x=1", "https://example.com/#x", "://"} {
		t.Run(value, func(t *testing.T) {
			if _, err := New(value, nil); err == nil {
				t.Fatalf("New(%q) succeeded", value)
			}
		})
	}
}

func TestNewPreservesEscapedDeploymentPath(t *testing.T) {
	tests := []struct {
		name        string
		serverURL   string
		wantEscaped string
	}{
		{"encoded slash", "https://paste.example/a%2Fb", "/a%2Fb/"},
		{"encoded trailing slash", "https://paste.example/a%2F", "/a%2F/"},
		{"encoded dot segments", "https://paste.example/%2E%2E/service", "/%2E%2E/service/"},
		{"encoded space", "https://paste.example/a%20b", "/a%20b/"},
		{"literal trailing slashes", "https://paste.example/base///", "/base/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotEscaped string
			client, err := New(tt.serverURL, &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				gotEscaped = request.URL.EscapedPath()
				_, _ = io.Copy(io.Discard, request.Body)
				return &http.Response{
					StatusCode: http.StatusSeeOther,
					Status:     "303 See Other",
					Header:     http.Header{"Location": []string{"https://paste.example/p/id"}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    request,
				}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Create(context.Background(), strings.NewReader("content"), time.Second); err != nil {
				t.Fatal(err)
			}
			if gotEscaped != tt.wantEscaped {
				t.Fatalf("escaped request path = %q, want %q", gotEscaped, tt.wantEscaped)
			}
		})
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

type errorTransport struct{ err error }

func (t errorTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.err }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type blockingReader struct {
	started chan struct{}
	release chan struct{}
}

func (r *blockingReader) Read([]byte) (int, error) {
	select {
	case <-r.started:
	default:
		close(r.started)
	}
	<-r.release
	return 0, io.EOF
}
