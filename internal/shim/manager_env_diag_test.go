package shim

import (
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/spawndiag"
)

// The shim baseline env is filtered at Manager construction. A var the gate
// refuses there is one the operator exported for the CLI and will never see, so
// the drop has to leave the package as a spawn diag — a log line alone is what
// #2412/#2493 were about.
//
// Not parallel: t.Setenv and the process-global diag observer.
func TestBaselineShimEnv_ReportsGateDrops(t *testing.T) {
	// A profile name with a space fails IsSafeProfileValue (the
	// credential_process injection guard) while staying an allowlisted key.
	t.Setenv("AWS_PROFILE", "not a valid profile")

	var mu sync.Mutex
	var got []spawndiag.Diag
	restore := spawndiag.Observe(func(scope string, d spawndiag.Diag) {
		if scope != shimEnvScope {
			return
		}
		mu.Lock()
		got = append(got, d)
		mu.Unlock()
	})
	defer restore()

	env := baselineShimEnv()

	for _, kv := range env {
		if strings.HasPrefix(kv, "AWS_PROFILE=") {
			t.Fatalf("unsafe AWS_PROFILE reached the shim env: %q", kv)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, d := range got {
		if d.Key != "AWS_PROFILE" {
			continue
		}
		found = true
		if d.Layer != "env-filter" || d.Action != "dropped" {
			t.Errorf("diag = layer %q action %q, want env-filter/dropped", d.Layer, d.Action)
		}
		if strings.Contains(d.Reason, "not a valid profile") {
			t.Errorf("reason echoed the value: %q", d.Reason)
		}
	}
	if !found {
		t.Errorf("AWS_PROFILE was dropped without a diag; diags seen: %+v", got)
	}
}
