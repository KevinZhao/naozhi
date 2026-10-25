package system

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// newDaemonHandlers builds Handlers with sysession disabled (nil Daemons) and a
// real Router, matching a dashboard-only deployment.
func newDaemonHandlers() *Handlers {
	return New(Deps{Router: session.NewRouter(session.RouterConfig{})})
}

func TestHandleDaemons_DisabledReturnsEmptyArray(t *testing.T) {
	t.Parallel()
	h := newDaemonHandlers()
	// Daemons intentionally nil — test the disabled-path contract (must still
	// return valid JSON array, not 404).

	r := httptest.NewRequest(http.MethodGet, "/api/system/daemons", nil)
	w := httptest.NewRecorder()
	h.HandleDaemons(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	body := strings.TrimSpace(w.Body.String())
	if body != "[]" {
		t.Errorf("body = %q, want %q", body, "[]")
	}
}

// TestHandleDaemons_JSONShape sanity-checks the top-level shape: the response
// is a JSON array (empty in unconfigured tests). A real Manager is exercised
// by the sysession package tests; this only locks the HTTP-layer contract.
func TestHandleDaemons_JSONShape(t *testing.T) {
	t.Parallel()
	h := newDaemonHandlers()

	r := httptest.NewRequest(http.MethodGet, "/api/system/daemons", nil)
	w := httptest.NewRecorder()
	h.HandleDaemons(w, r)

	var got []map[string]any
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode body: %v; body=%q", err, w.Body.String())
	}
	// Disabled Manager → empty list.
	if len(got) != 0 {
		t.Errorf("disabled handler: expected empty list, got %v", got)
	}
}
