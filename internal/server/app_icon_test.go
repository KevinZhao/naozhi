package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAppIcons_ServedAnonymously: browsers fetch manifest icons without
// credentials, so every icon manifest.json lists (and Safari's root
// apple-touch-icon.png) must answer 200 image/png in token mode, and a name
// outside the embedded table must not.
func TestAppIcons_ServedAnonymously(t *testing.T) {
	t.Parallel()
	srv := newTestServerWithToken(&mockPlatform{}, "app-icon-token")

	var m struct {
		Icons []struct{ Src string }
	}
	if err := json.Unmarshal(staticAssetBytes("manifest.json"), &m); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	if len(m.Icons) == 0 {
		t.Fatal("manifest.json lists no icons")
	}
	paths := []string{"/apple-touch-icon.png", "/icons/apple-touch-icon.png"}
	for _, ic := range m.Icons {
		paths = append(paths, ic.Src)
	}
	pngMagic := []byte("\x89PNG\r\n\x1a\n")
	for _, p := range paths {
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", p, w.Code)
			continue
		}
		if ct := w.Header().Get("Content-Type"); ct != "image/png" {
			t.Errorf("GET %s Content-Type = %q, want image/png", p, ct)
		}
		if !bytes.HasPrefix(w.Body.Bytes(), pngMagic) {
			t.Errorf("GET %s body is not a PNG", p)
		}
	}

	for _, p := range []string{"/icons/nope.png", "/icons/..%2fmanifest.json", "/icons/icon-512.svg"} {
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, w.Code)
		}
	}
}
