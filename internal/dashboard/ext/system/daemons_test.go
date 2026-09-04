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

func TestHandleClearLabelOrigin_RequiresKey(t *testing.T) {
	t.Parallel()
	h := newDaemonHandlers()

	cases := []struct {
		name string
		body string
		want int
	}{
		{"empty body", "", http.StatusBadRequest},
		{"missing key field", `{}`, http.StatusBadRequest},
		{"empty key", `{"key":""}`, http.StatusBadRequest},
		{"reserved (cron) key rejected", `{"key":"cron:foo"}`, http.StatusBadRequest},
		{"reserved (sys) key rejected", `{"key":"sys:auto-titler"}`, http.StatusBadRequest},
		{"reserved (project) key rejected", `{"key":"project:foo:planner"}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodPost,
				"/api/system/labels/clear-origin",
				strings.NewReader(c.body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.HandleClearLabelOrigin(w, r)
			if w.Code != c.want {
				t.Errorf("status = %d, want %d; body=%q", w.Code, c.want, w.Body.String())
			}
		})
	}
}

func TestHandleClearLabelOrigin_UnknownKeyReturns404(t *testing.T) {
	t.Parallel()
	h := newDaemonHandlers()

	body := `{"key":"feishu:direct:nobody:general"}`
	r := httptest.NewRequest(http.MethodPost,
		"/api/system/labels/clear-origin", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleClearLabelOrigin(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%q", w.Code, w.Body.String())
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
