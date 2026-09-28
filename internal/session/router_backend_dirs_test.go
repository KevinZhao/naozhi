package session

import "testing"

// TestNewRouter_KeepsItsOwnBackendDirs: the router's transcript directories
// are a copy, so a caller editing the map it passed in cannot redirect where
// history and resume checks look.
func TestNewRouter_KeepsItsOwnBackendDirs(t *testing.T) {
	dirs := map[string]string{"kiro": "/kiro/sessions"}
	r := NewRouter(RouterConfig{BackendDirs: dirs})
	t.Cleanup(r.Shutdown)
	dirs["kiro"] = "/elsewhere"
	if got := r.backendDirs["kiro"]; got != "/kiro/sessions" {
		t.Errorf("router kiro dir = %q after the caller's map changed, want /kiro/sessions", got)
	}
}
