package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWithMaxBytes_BoundsBody pins R246-ARCH-7 (#783): withMaxBytes wraps
// r.Body so a body that exceeds the cap returns the canonical
// http.MaxBytesReader error from Read instead of being decoded as-is. The
// test exercises the underlying contract: Reading past the cap yields a
// non-nil error and the bytes already returned never include the
// over-cap remainder.
func TestWithMaxBytes_BoundsBody(t *testing.T) {
	body := strings.Repeat("A", 1024)
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()

	limit := int64(64)
	wrapped := withMaxBytes(rec, req, limit)
	if wrapped == nil || wrapped.Body == nil {
		t.Fatal("withMaxBytes returned nil request or body")
	}

	got, err := io.ReadAll(wrapped.Body)
	if err == nil {
		t.Fatalf("ReadAll: want error reading past cap, got nil (read %d bytes)", len(got))
	}
	if int64(len(got)) > limit {
		t.Errorf("read %d bytes, must not exceed cap %d", len(got), limit)
	}
}

// TestWithMaxBytes_AllowsUnderCap pins the negative case: a body strictly
// under the cap reads successfully and returns the full content. Without
// this, a future edit that off-by-one'd the wrapper (e.g. n-1) would
// silently break the legitimate request path while passing the
// over-cap test above.
func TestWithMaxBytes_AllowsUnderCap(t *testing.T) {
	body := []byte("hello")
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	wrapped := withMaxBytes(rec, req, 1024)
	got, err := io.ReadAll(wrapped.Body)
	if err != nil {
		t.Fatalf("ReadAll under cap: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("got %q, want %q", got, body)
	}
}
