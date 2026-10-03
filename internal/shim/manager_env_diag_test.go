package shim

import (
	"context"
	"path/filepath"
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

// A per-spawn overlay entry the gate refuses is an access profile that did not
// take effect for this session, so StartShimWithBackend reports it under the
// session key's scope. naozhiBin points at a missing file: the spawn fails at
// cmd.Start, after the env is merged and the drops are emitted.
//
// Not parallel: t.Setenv and the process-global diag observer.
func TestStartShimWithBackend_ReportsOverlayGateDrops(t *testing.T) {
	// Keep SocketPath (and the stale-socket unlink) off ~/.naozhi/run.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	spawn := func(t *testing.T, key, profile string) []spawndiag.Diag {
		t.Helper()
		var mu sync.Mutex
		var got []spawndiag.Diag
		restore := spawndiag.Observe(func(scope string, d spawndiag.Diag) {
			if scope != key {
				return
			}
			mu.Lock()
			got = append(got, d)
			mu.Unlock()
		})
		defer restore()

		m := &Manager{
			maxShims:  1,
			stateDir:  t.TempDir(),
			naozhiBin: filepath.Join(t.TempDir(), "missing-naozhi"),
			cliPath:   "/bin/true",
			shims:     map[string]*ShimHandle{},
		}
		overlay := map[string]string{"AWS_PROFILE": profile}
		if _, err := m.StartShimWithBackend(context.Background(), key, "", "", nil, t.TempDir(), overlay, nil); err == nil {
			t.Fatal("StartShimWithBackend with a missing naozhi binary succeeded")
		}
		m.mu.Lock()
		pending := m.pendingShims
		m.mu.Unlock()
		if pending != 0 {
			t.Errorf("pendingShims = %d after the failed spawn, want 0", pending)
		}

		mu.Lock()
		defer mu.Unlock()
		var profileDiags []spawndiag.Diag
		for _, d := range got {
			if d.Key == "AWS_PROFILE" {
				profileDiags = append(profileDiags, d)
			}
		}
		return profileDiags
	}

	t.Run("refused value", func(t *testing.T) {
		got := spawn(t, "dashboard:direct:overlay-drop:general", "not a valid profile")
		if len(got) != 1 {
			t.Fatalf("AWS_PROFILE diags under the session scope = %+v, want exactly one", got)
		}
		d := got[0]
		if d.Layer != "env-filter" || d.Action != "dropped" {
			t.Errorf("diag = layer %q action %q, want env-filter/dropped", d.Layer, d.Action)
		}
		if strings.Contains(d.Reason, "not a valid profile") {
			t.Errorf("reason echoed the value: %q", d.Reason)
		}
	})

	t.Run("safe value", func(t *testing.T) {
		if got := spawn(t, "dashboard:direct:overlay-keep:general", "dev"); len(got) != 0 {
			t.Errorf("safe AWS_PROFILE overlay produced diags: %+v", got)
		}
	})
}
