package session

// router_backend_models_test.go — BackendModelManifest's two-tier resolution.
// docs/rfc/dashboard-model-effort-control.md §4.2.

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// manifestFakeProc is a TestProcess that also reports a model manifest,
// standing in for a live ACP process.
type manifestFakeProc struct {
	*TestProcess
	models []cli.ModelInfo
}

func (p *manifestFakeProc) AvailableModels() []cli.ModelInfo { return p.models }

func mkManifestRouter(t *testing.T) *Router {
	t.Helper()
	r := &Router{
		ss: sessionStore{sessions: make(map[string]*ManagedSession)},
	}
	r.bkStore.wrappers = map[string]*cli.Wrapper{
		"kiro":   cli.NewWrapper("/bin/false", &cli.ACPProtocol{BackendID: "kiro"}, "kiro"),
		"claude": cli.NewWrapper("/bin/false", &cli.ClaudeProtocol{}, "claude"),
	}
	r.bkStore.defaultBackend = "claude"
	r.bkStore.modelManifests = make(map[string][]cli.ModelInfo)
	r.bkStore.configuredModelLists = map[string][]string{
		"claude": {"sonnet", "opus", "haiku"},
	}
	return r
}

func TestBackendModelManifest_Tiers(t *testing.T) {
	t.Run("configured fallback when no live manifest", func(t *testing.T) {
		r := mkManifestRouter(t)
		got := r.BackendModelManifest("claude")
		if len(got) != 3 || got[0].ID != "sonnet" {
			t.Errorf("claude manifest = %v, want configured aliases", got)
		}
	})

	t.Run("runtime manifest wins and survives process death", func(t *testing.T) {
		r := mkManifestRouter(t)
		live := []cli.ModelInfo{{ID: "claude-fable-5"}, {ID: "claude-haiku-4.5"}}
		proc := &manifestFakeProc{TestProcess: NewTestProcess(), models: live}
		s := newSessionWithID("k1", "sess-1")
		s.SetBackend("kiro")
		s.storeProcess(proc)
		r.ss.sessions["k1"] = s

		got := r.BackendModelManifest("kiro")
		if len(got) != 2 || got[0].ID != "claude-fable-5" {
			t.Fatalf("kiro manifest = %v, want live-reported list", got)
		}

		// Process recycled: cached copy still serves.
		proc.AliveVal = false
		got = r.BackendModelManifest("kiro")
		if len(got) != 2 {
			t.Errorf("manifest lost after process death: %v", got)
		}
	})

	t.Run("empty id resolves default backend", func(t *testing.T) {
		r := mkManifestRouter(t)
		got := r.BackendModelManifest("")
		if len(got) != 3 {
			t.Errorf("default-backend manifest = %v, want claude aliases", got)
		}
	})

	t.Run("unknown backend yields nil (popover manual-input fallback)", func(t *testing.T) {
		r := mkManifestRouter(t)
		if got := r.BackendModelManifest("codex"); got != nil {
			t.Errorf("manifest = %v, want nil", got)
		}
	})
}
