package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAcceptsGzip(t *testing.T) {
	tests := []struct {
		header string
		want   bool
	}{
		{"gzip", true},
		{"gzip, deflate", true},
		{"deflate, gzip;q=1.0, *;q=0.5", true},
		{"GZIP", true},
		{"deflate", false},
		{"", false},
		{"identity", false},
	}
	for _, tc := range tests {
		if got := acceptsGzip(tc.header); got != tc.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

func TestIsCompressibleType(t *testing.T) {
	tests := []struct {
		ct   string
		want bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"application/manifest+json", true},
		{"text/html; charset=utf-8", true},
		{"text/plain", true},
		{"application/javascript", true},
		{"application/xml", true},
		{"image/png", false},
		{"image/jpeg", false},
		{"application/octet-stream", false},
		{"audio/webm", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := isCompressibleType(tc.ct); got != tc.want {
			t.Errorf("isCompressibleType(%q) = %v, want %v", tc.ct, got, tc.want)
		}
	}
}

func TestGzipMiddlewareCompressesJSON(t *testing.T) {
	// Repeated payload so gzip shows meaningful compression.
	payload := strings.Repeat(`{"type":"assistant","text":"hello world"}`, 100)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/events", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	gzipMiddleware(h).ServeHTTP(w, req)

	resp := w.Result()
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := resp.Header.Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("Vary = %q, want to contain Accept-Encoding", got)
	}
	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	decoded, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	if string(decoded) != payload {
		t.Errorf("decoded body mismatch")
	}
	if len(w.Body.Bytes()) >= len(payload) {
		t.Errorf("compressed size %d not smaller than raw %d", len(w.Body.Bytes()), len(payload))
	}
}

func TestGzipMiddlewareSkipsBinary(t *testing.T) {
	payload := bytes.Repeat([]byte{0xff, 0xd8, 0xff}, 50) // fake JPEG-ish bytes
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(payload)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/projects/file", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	gzipMiddleware(h).ServeHTTP(w, req)

	resp := w.Result()
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty for image/jpeg", got)
	}
	if !bytes.Equal(w.Body.Bytes(), payload) {
		t.Errorf("body was altered for binary response")
	}
}

func TestGzipMiddlewareSkipsWithoutAcceptEncoding(t *testing.T) {
	payload := strings.Repeat("x", 500)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	// No Accept-Encoding header
	w := httptest.NewRecorder()
	gzipMiddleware(h).ServeHTTP(w, req)

	resp := w.Result()
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty", got)
	}
	if w.Body.String() != payload {
		t.Errorf("body was altered when client did not accept gzip")
	}
}

func TestGzipMiddlewareBypassesWebSocketUpgrade(t *testing.T) {
	// A WS upgrade handler expects to Hijack the conn; wrapping it would break
	// that contract. The middleware must leave the original ResponseWriter
	// untouched when Upgrade: websocket is set.
	var gotRW http.ResponseWriter
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRW = w
	})

	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	gzipMiddleware(h).ServeHTTP(w, req)

	if _, wrapped := gotRW.(*gzipResponseWriter); wrapped {
		t.Errorf("ResponseWriter was wrapped for WS upgrade; Hijacker would be lost")
	}
}

func TestGzipMiddlewarePreservesPreEncodedContent(t *testing.T) {
	// Handlers that already set Content-Encoding (e.g. serving pre-gzipped
	// assets) must not be double-encoded.
	payload := []byte("already-compressed-bytes")
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(payload)
	})

	req := httptest.NewRequest(http.MethodGet, "/pre-compressed", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	gzipMiddleware(h).ServeHTTP(w, req)

	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}
	if !bytes.Equal(w.Body.Bytes(), payload) {
		t.Errorf("pre-encoded body was re-compressed")
	}
}
