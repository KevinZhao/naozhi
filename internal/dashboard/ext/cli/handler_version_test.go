package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	clipkg "github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// decodeBackends runs the handler and returns the parsed "backends" list.
func decodeBackends(t *testing.T, h *Handler) []clipkg.BackendInfo {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cli/backends", nil)
	h.Handle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Backends []clipkg.BackendInfo `json:"backends"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body.Backends
}

func findBackend(list []clipkg.BackendInfo, id string) (clipkg.BackendInfo, bool) {
	for _, b := range list {
		if b.ID == id {
			return b, true
		}
	}
	return clipkg.BackendInfo{}, false
}

// routerWithWrapper builds a single-wrapper router whose lone backend is
// "claude" with the given spawn-time CLIVersion.
func routerWithWrapper(spawnVersion string) (*session.Router, *clipkg.Wrapper) {
	w := clipkg.NewWrapper("/nonexistent/cli-binary", &clipkg.ClaudeProtocol{}, "claude")
	w.CLIVersion = spawnVersion
	r := session.NewRouter(session.RouterConfig{Wrapper: w})
	return r, w
}

// TestHandle_Version_PrefersLiveObserved pins R20260613-pending-version: the
// /api/cli/backends version field must surface the live init-frame version
// once a spawned process has reported it, not the stale spawn-time CLIVersion.
// The dashboard's pending-session card falls back to this value via
// backendDisplayVersion(), so a host claude upgrade under a long-lived naozhi
// must not leave a freshly-created card showing the pre-upgrade version.
func TestHandle_Version_PrefersLiveObserved(t *testing.T) {
	r, w := routerWithWrapper("2.1.100")
	h := &Handler{router: r}

	// Before any process reports: spawn-time version.
	got, ok := findBackend(decodeBackends(t, h), "claude")
	if !ok {
		t.Fatal("claude backend missing from response")
	}
	if got.Version != "2.1.100" {
		t.Fatalf("version before live observe = %q, want spawn-time 2.1.100", got.Version)
	}
	if !got.Available {
		t.Fatal("backend should be Available when version is non-empty")
	}

	// A spawned process reports the binary it actually exec'd (newer after a
	// host upgrade). The endpoint must now surface that live value.
	w.ObserveLiveVersion("2.1.176")
	got, _ = findBackend(decodeBackends(t, h), "claude")
	if got.Version != "2.1.176" {
		t.Fatalf("version after live observe = %q, want live 2.1.176", got.Version)
	}
}

// TestHandle_Version_AvailableTracksEffective guards the Available flag: it is
// derived from EffectiveVersion, not the spawn-time CLIVersion, so a wrapper
// that detected nothing at startup but later observed a live version flips to
// Available.
func TestHandle_Version_AvailableTracksEffective(t *testing.T) {
	r, w := routerWithWrapper("") // spawn-time detection failed
	h := &Handler{router: r}

	got, ok := findBackend(decodeBackends(t, h), "claude")
	if !ok {
		t.Fatal("claude backend missing from response")
	}
	if got.Available {
		t.Fatal("backend with empty effective version must not be Available")
	}

	w.ObserveLiveVersion("2.1.176")
	got, _ = findBackend(decodeBackends(t, h), "claude")
	if !got.Available || got.Version != "2.1.176" {
		t.Fatalf("after live observe: Available=%v version=%q, want true/2.1.176", got.Available, got.Version)
	}
}
