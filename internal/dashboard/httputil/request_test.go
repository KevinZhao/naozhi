package httputil

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestWithMaxBytes_CapsBody(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 16)))
	r = WithMaxBytes(w, r, 8)
	_, err := io.ReadAll(r.Body)
	var mbe *http.MaxBytesError
	if !errors.As(err, &mbe) {
		t.Fatalf("ReadAll err = %v, want *http.MaxBytesError", err)
	}
	if mbe.Limit != 8 {
		t.Fatalf("MaxBytesError.Limit = %d, want 8", mbe.Limit)
	}
}

func TestWithMaxBytes_UnderCapPassesThrough(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello"))
	got, err := io.ReadAll(WithMaxBytes(w, r, 8).Body)
	if err != nil || string(got) != "hello" {
		t.Fatalf("ReadAll = %q, %v; want hello, nil", got, err)
	}
}

func multipartWithFields(n int) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.MultipartForm = &multipart.Form{Value: map[string][]string{}}
	for i := 0; i < n; i++ {
		r.MultipartForm.Value["f"+strconv.Itoa(i)] = []string{"v"}
	}
	return r
}

func TestRejectIfTooManyFields(t *testing.T) {
	cases := []struct {
		name   string
		req    *http.Request
		reject bool
	}{
		{"nil form", httptest.NewRequest(http.MethodPost, "/", nil), false},
		{"empty form", multipartWithFields(0), false},
		{"at cap", multipartWithFields(MaxMultipartFields), false},
		{"over cap", multipartWithFields(MaxMultipartFields + 1), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			if got := RejectIfTooManyFields(w, tc.req); got != tc.reject {
				t.Fatalf("reject = %v, want %v", got, tc.reject)
			}
			if !tc.reject {
				if w.Code != http.StatusOK || w.Body.Len() != 0 {
					t.Fatalf("expected nothing written, got %d %q", w.Code, w.Body.String())
				}
				return
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
			if !strings.Contains(w.Body.String(), `"error":"too many form fields"`) {
				t.Fatalf("body = %q", w.Body.String())
			}
		})
	}
}

// TestRejectIfTooManyFields_CountsRepeatedValues pins that the cap counts
// individual values, not distinct keys — a single key repeated past the cap
// must still be rejected.
func TestRejectIfTooManyFields_CountsRepeatedValues(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	vals := make([]string, MaxMultipartFields+1)
	r.MultipartForm = &multipart.Form{Value: map[string][]string{"k": vals}}
	w := httptest.NewRecorder()
	if !RejectIfTooManyFields(w, r) {
		t.Fatal("expected rejection for repeated key exceeding cap")
	}
}
