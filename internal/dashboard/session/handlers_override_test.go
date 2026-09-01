package session

// handlers_override_test.go — HTTP contract for POST /api/sessions/override.
// docs/rfc/dashboard-model-effort-control.md §5 (API / 校验 rows).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	sessionpkg "github.com/naozhi/naozhi/internal/session"
)

func newOverrideHandler(t *testing.T, key string) *Handlers {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	r := sessionpkg.NewRouter(sessionpkg.RouterConfig{MaxProcs: 4, StorePath: storePath})
	t.Cleanup(r.Shutdown)
	if key != "" {
		// Suspended session (no live proc): overrides take the deferred path,
		// which is all the HTTP contract needs — path selection itself is
		// covered by router_tuning_test.go.
		r.InjectSession(key, &sessionpkg.TestProcess{AliveVal: false})
	}
	return New(Deps{Router: r})
}

func doOverride(t *testing.T, h *Handlers, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/override", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.HandleOverride(rec, req)
	return rec.Result()
}

func TestHandleOverride_Contract(t *testing.T) {
	const key = "feishu:p2p:tuner"

	t.Run("success returns applied_via", func(t *testing.T) {
		h := newOverrideHandler(t, key)
		res := doOverride(t, h, `{"key":"`+key+`","model":"claude-haiku-4.5"}`)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}
		var body map[string]string
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["applied_via"] != sessionpkg.TuningAppliedDeferred {
			t.Errorf("applied_via = %q, want deferred (suspended session)", body["applied_via"])
		}
	})

	t.Run("missing key is 400", func(t *testing.T) {
		h := newOverrideHandler(t, key)
		if res := doOverride(t, h, `{"model":"opus"}`); res.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", res.StatusCode)
		}
	})

	t.Run("invalid key bytes are 400", func(t *testing.T) {
		h := newOverrideHandler(t, key)
		if res := doOverride(t, h, `{"key":"a\u0000b","model":"opus"}`); res.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", res.StatusCode)
		}
	})

	t.Run("flag-shaped model is 400", func(t *testing.T) {
		h := newOverrideHandler(t, key)
		if res := doOverride(t, h, `{"key":"`+key+`","model":"--inject"}`); res.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", res.StatusCode)
		}
	})

	t.Run("out-of-set effort is 400", func(t *testing.T) {
		h := newOverrideHandler(t, key)
		if res := doOverride(t, h, `{"key":"`+key+`","effort":"ultra"}`); res.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", res.StatusCode)
		}
	})

	t.Run("effort on non-EffortTier backend is 400", func(t *testing.T) {
		// Router default backend in tests is claude (stream-json) — no tier.
		h := newOverrideHandler(t, key)
		if res := doOverride(t, h, `{"key":"`+key+`","effort":"high"}`); res.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", res.StatusCode)
		}
	})

	t.Run("unknown session is 404", func(t *testing.T) {
		h := newOverrideHandler(t, "")
		if res := doOverride(t, h, `{"key":"feishu:p2p:ghost","model":"opus"}`); res.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", res.StatusCode)
		}
	})

	t.Run("remote node is 501 (NG4)", func(t *testing.T) {
		h := newOverrideHandler(t, key)
		if res := doOverride(t, h, `{"key":"`+key+`","node":"worknode","model":"opus"}`); res.StatusCode != http.StatusNotImplemented {
			t.Errorf("status = %d, want 501", res.StatusCode)
		}
	})

	t.Run("no fields is 400", func(t *testing.T) {
		h := newOverrideHandler(t, key)
		if res := doOverride(t, h, `{"key":"`+key+`"}`); res.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", res.StatusCode)
		}
	})
}
