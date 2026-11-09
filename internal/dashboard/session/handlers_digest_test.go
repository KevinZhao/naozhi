package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

type fakeDigests map[string]string

func (f fakeDigests) Read(key string) (time.Time, []byte, bool) {
	md, ok := f[key]
	return time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC), []byte(md), ok
}

// GET /api/sessions/digest returns a session's digest, 404 for one without
// (or with the digests off), 400 for a key that is not one.
func TestHandleDigest(t *testing.T) {
	t.Parallel()
	get := func(src DigestSource, key string) *httptest.ResponseRecorder {
		h := New(Deps{Digests: src})
		w := httptest.NewRecorder()
		h.HandleDigest(w, httptest.NewRequest(http.MethodGet, "/api/sessions/digest?key="+url.QueryEscape(key), nil))
		return w
	}
	src := fakeDigests{"task:abc": "---\nkey: \"task:abc\"\n---\n# t\n"}
	w := get(src, "task:abc")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var body struct {
		Key      string    `json:"key"`
		Updated  time.Time `json:"updated"`
		Markdown string    `json:"markdown"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Key != "task:abc" || body.Markdown != src["task:abc"] || body.Updated.IsZero() {
		t.Errorf("body = %+v", body)
	}
	if w := get(src, "task:none"); w.Code != http.StatusNotFound {
		t.Errorf("no digest: status %d", w.Code)
	}
	if w := get(nil, "task:abc"); w.Code != http.StatusNotFound {
		t.Errorf("digests off: status %d", w.Code)
	}
	if w := get(src, ""); w.Code != http.StatusBadRequest {
		t.Errorf("empty key: status %d", w.Code)
	}
}
