package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseTTL(t *testing.T) {
	tests := []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"604800", 7 * 24 * time.Hour, true},
		{"1h", time.Hour, true},
		{"1.5h", 90 * time.Minute, true},
		{"168h", 7 * 24 * time.Hour, true},
		{"0", 0, false},
		{"-1", 0, false},
		{"1.5s", 0, false},
		{"later", 0, false},
		{"9223372036854775807", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := parseTTL(tt.value)
			if (err == nil) != tt.ok || got != tt.want {
				t.Fatalf("parseTTL(%q) = %s, %v", tt.value, got, err)
			}
		})
	}
}

func TestTruncateValue(t *testing.T) {
	tests := []struct {
		value   string
		want    int64
		enabled bool
		ok      bool
	}{
		{"true", maximumBytes, true, true},
		{"false", maximumBytes, false, true},
		{"100", 100, true, true},
		{"0", 0, false, false},
		{"-1", 0, false, false},
		{"21", 0, false, false},
		{"not-bytes", 0, false, false},
		{"9223372036854775808", 0, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			value := truncateValue{limit: maximumBytes}
			err := value.Set(tt.value)
			if (err == nil) != tt.ok {
				t.Fatalf("Set(%q) error = %v", tt.value, err)
			}
			if tt.ok && (value.limit != tt.want || value.enabled != tt.enabled) {
				t.Fatalf("Set(%q) = %+v", tt.value, value)
			}
		})
	}
}

func TestTruncatingReader(t *testing.T) {
	const limit = int64(32)
	markerSize := int64(len(truncateMarker))
	tests := []struct {
		name          string
		input         string
		want          string
		wantTruncated bool
	}{
		{"under limit", "short text", "short text", false},
		{"exact limit", strings.Repeat("x", int(limit)), strings.Repeat("x", int(limit)), false},
		{"over limit", strings.Repeat("x", int(limit+1)), strings.Repeat("x", int(limit-markerSize)) + truncateMarker, true},
		{"well over limit", strings.Repeat("x", 1000), strings.Repeat("x", int(limit-markerSize)) + truncateMarker, true},
		{"rune boundary", strings.Repeat("x", int(limit-markerSize-1)) + "é" + strings.Repeat("y", 30), strings.Repeat("x", int(limit-markerSize-1)) + truncateMarker, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			truncated := 0
			got, err := io.ReadAll(newTruncatingReader(strings.NewReader(tt.input), limit, func() { truncated++ }))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("output = %q, want %q", got, tt.want)
			}
			if len(got) > int(limit) {
				t.Fatalf("output length = %d, limit = %d", len(got), limit)
			}
			wantCalls := 0
			if tt.wantTruncated {
				wantCalls = 1
			}
			if truncated != wantCalls {
				t.Fatalf("truncate callback calls = %d, want %d", truncated, wantCalls)
			}
		})
	}
}

func TestTruncatingReaderAtMinimumLimit(t *testing.T) {
	limit := int64(len(truncateMarker))
	got, err := io.ReadAll(newTruncatingReader(strings.NewReader(strings.Repeat("x", int(limit+1))), limit, nil))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != truncateMarker || int64(len(got)) != limit {
		t.Fatalf("output = %q (length %d), want marker of length %d", got, len(got), limit)
	}
}

func TestTruncatingReaderErrors(t *testing.T) {
	if _, err := io.ReadAll(newTruncatingReader(bytes.NewReader([]byte{'a', 0xff, 'b'}), 32, nil)); !errors.Is(err, errTruncateNonUTF8) {
		t.Fatalf("binary error = %v", err)
	}
	sentinel := errors.New("source failed")
	if _, err := io.ReadAll(newTruncatingReader(errorReader{err: sentinel}, 32, nil)); !errors.Is(err, sentinel) {
		t.Fatalf("reader error = %v", err)
	}
}

func TestRunStdinAndFlags(t *testing.T) {
	server, received := pasteServer(t)
	defer server.Close()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--server", server.URL + "/base", "--ttl", "1h"}, strings.NewReader("from stdin"), &stdout, &stderr, noEnvironment, server.Client())
	if code != 0 || stdout.String() != server.URL+"/base/p/id\n" || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	got := <-received
	if got.content != "from stdin" || got.ttl != "3600" || got.path != "/base/" {
		t.Fatalf("request = %+v", got)
	}
}

func TestRunExplicitStdinAndDefaultTTL(t *testing.T) {
	server, received := pasteServer(t)
	defer server.Close()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--server", server.URL, "-"}, strings.NewReader("explicit stdin"), &stdout, &stderr, noEnvironment, server.Client())
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	got := <-received
	if got.content != "explicit stdin" || got.ttl != "604800" {
		t.Fatalf("request = %+v", got)
	}
}

func TestRunFileAndBinaryContent(t *testing.T) {
	server, received := pasteServer(t)
	defer server.Close()
	filename := filepath.Join(t.TempDir(), "paste.bin")
	want := []byte{'a', 0, 'b', 0xff}
	if err := os.WriteFile(filename, want, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--server", server.URL, filename}, strings.NewReader("ignored"), &stdout, &stderr, noEnvironment, server.Client())
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if got := []byte((<-received).content); !bytes.Equal(got, want) {
		t.Fatalf("content = %v, want %v", got, want)
	}
}

func TestRunContentSizeWarning(t *testing.T) {
	for _, tt := range []struct {
		name     string
		size     int64
		wantWarn bool
	}{
		{"at limit", maximumBytes, false},
		{"over limit", maximumBytes + 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			transport := &captureTransport{}
			var stdout, stderr bytes.Buffer
			content := io.LimitReader(zeroReader{}, tt.size)
			code := run(context.Background(), []string{"--server", "https://paste.example/"}, content, &stdout, &stderr, noEnvironment, &http.Client{Transport: transport})
			if code != 0 {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			warned := strings.Contains(stderr.String(), "warning: content exceeds 307200 bytes")
			if warned != tt.wantWarn {
				t.Fatalf("warning present = %t, want %t; stderr=%q", warned, tt.wantWarn, stderr.String())
			}
		})
	}
}

func TestRunLargeFileWarnsOnce(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "large.txt")
	if err := os.WriteFile(filename, bytes.Repeat([]byte{'x'}, int(maximumBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--server", "https://paste.example/", filename}, strings.NewReader(""), &stdout, &stderr, noEnvironment, &http.Client{Transport: &captureTransport{}})
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if got := strings.Count(stderr.String(), "warning: content exceeds 307200 bytes"); got != 1 {
		t.Fatalf("warning count = %d, stderr=%q", got, stderr.String())
	}
}

func TestRunTruncation(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		input     string
		want      string
		wantWarns []string
	}{
		{
			name:  "bare default",
			args:  []string{"--truncate"},
			input: strings.Repeat("x", int(maximumBytes+1)),
			want:  strings.Repeat("x", int(maximumBytes)-len(truncateMarker)) + truncateMarker,
		},
		{
			name:  "custom limit",
			args:  []string{"--truncate=32"},
			input: strings.Repeat("x", 33),
			want:  strings.Repeat("x", 32-len(truncateMarker)) + truncateMarker,
		},
		{
			name:      "custom limit above server default",
			args:      []string{"--truncate=307222"},
			input:     strings.Repeat("x", 307223),
			want:      strings.Repeat("x", 307222-len(truncateMarker)) + truncateMarker,
			wantWarns: []string{"warning: content exceeds 307200 bytes"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			transport := &recordingTransport{}
			args := append([]string{"--server", "https://paste.example/"}, tt.args...)
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), args, strings.NewReader(tt.input), &stdout, &stderr, noEnvironment, &http.Client{Transport: transport})
			if code != 0 {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if transport.content != tt.want {
				t.Fatalf("content length = %d, want %d", len(transport.content), len(tt.want))
			}
			for _, warning := range tt.wantWarns {
				if strings.Count(stderr.String(), warning) != 1 {
					t.Errorf("warning %q count != 1; stderr=%q", warning, stderr.String())
				}
			}
			if strings.Contains(stderr.String(), "warning: content truncated") {
				t.Errorf("unexpected truncation warning: stderr=%q", stderr.String())
			}
		})
	}
}

func TestRunBareTruncateDoesNotConsumeFilename(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "paste.txt")
	if err := os.WriteFile(filename, []byte("file content"), 0o600); err != nil {
		t.Fatal(err)
	}
	transport := &recordingTransport{}
	var stdout, stderr bytes.Buffer
	args := []string{"--server", "https://paste.example/", "--truncate", filename}
	if code := run(context.Background(), args, strings.NewReader("stdin content"), &stdout, &stderr, noEnvironment, &http.Client{Transport: transport}); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if transport.content != "file content" {
		t.Fatalf("content = %q", transport.content)
	}
}

func TestRunTruncateRejectsBinary(t *testing.T) {
	transport := &recordingTransport{}
	var stdout, stderr bytes.Buffer
	input := bytes.NewReader([]byte{'a', 0xff, 'b'})
	code := run(context.Background(), []string{"--server", "https://paste.example/", "--truncate"}, input, &stdout, &stderr, noEnvironment, &http.Client{Transport: transport})
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "--truncate supports UTF-8 text only") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunServerPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     string
		wantURL string
	}{
		{"default", nil, "", defaultServer},
		{"environment", nil, "https://env.example/base", "https://env.example/base/"},
		{"flag", []string{"--server", "https://flag.example/path"}, "https://env.example/", "https://flag.example/path/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := &captureTransport{}
			httpClient := &http.Client{Transport: transport}
			getenv := func(key string) string {
				if key == "SIXPASTER_SERVER" {
					return tt.env
				}
				return ""
			}
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), tt.args, strings.NewReader("x"), &stdout, &stderr, getenv, httpClient); code != 0 {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if transport.url != tt.wantURL {
				t.Fatalf("request URL = %q, want %q", transport.url, tt.wantURL)
			}
		})
	}
}

func TestRunErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"bad ttl", []string{"--ttl", "later"}, 2},
		{"bad server", []string{"--server", "not-a-url"}, 2},
		{"too many files", []string{"one", "two"}, 2},
		{"unknown flag", []string{"--unknown"}, 2},
		{"bad truncate", []string{"--truncate=invalid"}, 2},
		{"zero truncate", []string{"--truncate=0"}, 2},
		{"small truncate", []string{"--truncate=21"}, 2},
		{"overflow truncate", []string{"--truncate=9223372036854775808"}, 2},
		{"missing file", []string{filepath.Join(t.TempDir(), "missing")}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(context.Background(), tt.args, strings.NewReader("x"), &stdout, &stderr, noEnvironment, &http.Client{Transport: &captureTransport{}}); got != tt.want {
				t.Fatalf("code=%d, want %d; stderr=%q", got, tt.want, stderr.String())
			}
			if stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunServiceAndReaderErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "no paste", http.StatusBadRequest)
	}))
	defer server.Close()
	for _, stdin := range []io.Reader{strings.NewReader(""), failingReader{}} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"--server", server.URL}, stdin, &stdout, &stderr, noEnvironment, server.Client())
		if code != 1 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	}
}

func TestRunEscapesServiceErrorControls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "rejected\x1b]52;c;YXR0YWNr\a\r\nforged")
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--server", server.URL}, strings.NewReader("content"), &stdout, &stderr, noEnvironment, server.Client())
	if code != 1 || stdout.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.ContainsAny(stderr.String(), "\x1b\a\r") || strings.Count(stderr.String(), "\n") != 1 {
		t.Fatalf("stderr contains terminal controls: %q", stderr.String())
	}
	for _, escaped := range []string{`\x1b`, `\a`, `\r`, `\n`} {
		if !strings.Contains(stderr.String(), escaped) {
			t.Errorf("stderr does not contain escaped control %q: %q", escaped, stderr.String())
		}
	}
}

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, strings.NewReader(""), &stdout, &stderr, noEnvironment, nil); code != 0 || !strings.Contains(stderr.String(), "Usage:") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

type receivedPaste struct {
	path, content, ttl string
}

func pasteServer(t *testing.T) (*httptest.Server, <-chan receivedPaste) {
	t.Helper()
	received := make(chan receivedPaste, 1)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		received <- receivedPaste{path: r.URL.Path, content: r.FormValue("content"), ttl: r.FormValue("ttl")}
		http.Redirect(w, r, r.URL.Path+"p/id", http.StatusSeeOther)
	}))
	return server, received
}

func noEnvironment(string) string { return "" }

type captureTransport struct{ url string }

func (t *captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.url = r.URL.String()
	_, _ = io.Copy(io.Discard, r.Body)
	location, _ := url.Parse("https://result.example/p/id")
	return &http.Response{
		StatusCode: http.StatusSeeOther,
		Status:     "303 See Other",
		Header:     http.Header{"Location": []string{location.String()}},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    r,
	}, nil
}

type recordingTransport struct {
	content string
}

func (t *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return nil, err
	}
	t.content = r.FormValue("content")
	return &http.Response{
		StatusCode: http.StatusSeeOther,
		Status:     "303 See Other",
		Header:     http.Header{"Location": []string{"https://result.example/p/id"}},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    r,
	}, nil
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
