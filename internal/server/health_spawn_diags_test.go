package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/spawndiag"
)

// spawn_diags reaches the authenticated /health, with the key and never the
// refused value or the session key it was refused for.
func TestHealthProbe_SpawnDiags(t *testing.T) {
	spawndiag.One("dashboard:direct:chat-sentinel-7:general", "env-filter", "HTTPS_PROXY", "dropped",
		"value fails its guard: http://value-sentinel-7")
	// Through the handler's probe list, so an unregistered probe fails too.
	auth := &healthAuthSection{}
	h := &HealthHandler{dispatcherMetrics: func() (int64, int64, int64, time.Time) { return 0, 0, 0, time.Time{} }}
	for _, probe := range h.subsystemProbes() {
		probe(auth)
	}
	if auth.SpawnDiags == nil || auth.SpawnDiags.Counts["env-filter|dropped"] < 1 {
		t.Fatalf("spawn_diags = %+v, want the env-filter drop counted", auth.SpawnDiags)
	}
	b, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"HTTPS_PROXY"`) {
		t.Errorf("the refused key is missing: %s", b)
	}
	for _, leak := range []string{"value-sentinel-7", "chat-sentinel-7"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("/health carries %q: %s", leak, b)
		}
	}
}
