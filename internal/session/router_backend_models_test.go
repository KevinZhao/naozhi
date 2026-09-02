package session

// router_backend_models_test.go — BackendModelManifest's three-tier resolution.
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

// TestBackendModelManifest_ObservedTier pins the third tier: with neither an
// agent-reported nor a configured list, the popover still gets the models the
// deployment has actually used for that backend (router default, live session
// models, tuning overrides) — deduped, default first, rest sorted — and that
// tier never leaks into a backend that has a configured list.
func TestBackendModelManifest_ObservedTier(t *testing.T) {
	addSess := func(r *Router, key, backend, model, tuning string) {
		s := newSessionWithID(key, "sess-"+key)
		s.SetBackend(backend)
		s.SetModel(model)
		s.SetTuningModel(tuning)
		r.ss.sessions[key] = s
	}

	t.Run("observed models when no runtime and no config", func(t *testing.T) {
		r := mkManifestRouter(t)
		r.bkStore.configuredModelLists = map[string][]string{}
		r.bkStore.model = "us.anthropic.default"
		addSess(r, "k1", "claude", "sonnet", "")
		addSess(r, "k2", "claude", "sonnet", "opus") // dup model + tuning
		addSess(r, "k3", "", "haiku", "")            // empty backend → default (claude)
		addSess(r, "k4", "kiro", "kiro-only", "")    // other backend must not leak

		got := r.BackendModelManifest("claude")
		want := []string{"us.anthropic.default", "haiku", "opus", "sonnet"}
		if len(got) != len(want) {
			t.Fatalf("observed manifest = %v, want %v", got, want)
		}
		for i := range want {
			if got[i].ID != want[i] {
				t.Errorf("observed[%d] = %q, want %q (full %v)", i, got[i].ID, want[i], got)
			}
		}
	})

	t.Run("observed tier is stable across calls", func(t *testing.T) {
		r := mkManifestRouter(t)
		r.bkStore.configuredModelLists = map[string][]string{}
		for _, m := range []string{"c", "a", "b", "d", "e"} {
			addSess(r, "k-"+m, "claude", m, "")
		}
		first := r.BackendModelManifest("claude")
		for i := 0; i < 5; i++ {
			again := r.BackendModelManifest("claude")
			for j := range first {
				if again[j].ID != first[j].ID {
					t.Fatalf("order changed between calls: %v vs %v", first, again)
				}
			}
		}
	})

	t.Run("configured list wins over observed models", func(t *testing.T) {
		r := mkManifestRouter(t)
		addSess(r, "k1", "claude", "us.anthropic.claude-fable-5-1", "")
		got := r.BackendModelManifest("claude")
		if len(got) != 3 {
			t.Fatalf("manifest = %v, want the 3 configured aliases only", got)
		}
		for _, m := range got {
			if m.ID == "us.anthropic.claude-fable-5-1" {
				t.Errorf("observed model leaked into configured tier: %v", got)
			}
		}
	})

	t.Run("nothing observed yields nil", func(t *testing.T) {
		r := mkManifestRouter(t)
		r.bkStore.configuredModelLists = map[string][]string{}
		addSess(r, "k1", "claude", "", "")
		if got := r.BackendModelManifest("claude"); got != nil {
			t.Errorf("manifest = %v, want nil", got)
		}
	})
}
