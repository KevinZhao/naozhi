package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/shim"
)

// TestOverlayDriftFields covers the per-field diff: named-field drift,
// equal argv → nil, and the {Field:"args"} fallback for backends whose argv
// carries no --model/--effort tokens (codex renders model as `-c model=X`).
func TestOverlayDriftFields(t *testing.T) {
	t.Parallel()
	stored := []string{"-p", "--model", "claude-opus-4.7", "--effort", "high"}
	current := []string{"-p", "--model", "claude-fable-5", "--effort", "high"}
	got := overlayDriftFields(stored, current)
	want := []OverlayFieldDrift{{Field: "model", Stored: "claude-opus-4.7", Current: "claude-fable-5"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("model drift = %+v, want %+v", got, want)
	}

	if d := overlayDriftFields(stored, stored); d != nil {
		t.Errorf("equal argv must yield nil, got %+v", d)
	}

	codexStored := []string{"exec", "-c", "model=gpt-5.3-codex", "--json"}
	codexCurrent := []string{"exec", "-c", "model=gpt-6", "--json"}
	got = overlayDriftFields(codexStored, codexCurrent)
	if len(got) != 1 || got[0].Field != "args" {
		t.Errorf("codex-shaped drift = %+v, want the args fallback entry", got)
	}
}

// TestOverlayDriftFields_RoundTripsBuildArgs pins the parser against what
// ClaudeProtocol.BuildArgs actually renders, so the flag-extraction rule and
// the renderer cannot drift apart.
func TestOverlayDriftFields_RoundTripsBuildArgs(t *testing.T) {
	t.Parallel()
	p := &cli.ClaudeProtocol{}
	args := p.BuildArgs(cli.SpawnOptions{
		Model:              "claude-fable-5",
		Effort:             "xhigh",
		AppendSystemPrompt: "保持简短。",
	})
	if got := argvFlagValue(args, "--model"); got != "claude-fable-5" {
		t.Errorf("--model round-trip = %q", got)
	}
	if got := argvFlagValue(args, "--effort"); got != "xhigh" {
		t.Errorf("--effort round-trip = %q", got)
	}
	if got := argvFlagValue(args, "--append-system-prompt"); got != "保持简短。" {
		t.Errorf("--append-system-prompt round-trip = %q", got)
	}
	if got := argvFlagValue(args, "--resume"); got != "" {
		t.Errorf("absent flag = %q, want empty", got)
	}
}

// TestSnapshot_OverlayDriftExposed pins the /api/sessions surface: drift set
// by the reconciler rides the snapshot, and a clean session still serialises
// overlay_drift as an empty array (same contract as spawn_diags).
func TestSnapshot_OverlayDriftExposed(t *testing.T) {
	r := newTestRouter(4)
	drifted := injectSession(r, "drift:sess", &fakeProcess{})
	d := []OverlayFieldDrift{{Field: "model", Stored: "a", Current: "b"}}
	drifted.overlayDrift.Store(&d)
	injectSession(r, "clean:sess", &fakeProcess{})

	for _, snap := range r.ListSessions() {
		data, err := json.Marshal(snap)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"overlay_drift":[`) {
			t.Errorf("snapshot %q lacks overlay_drift array: %s", snap.Key, data)
		}
		switch snap.Key {
		case "drift:sess":
			if len(snap.OverlayDrift) != 1 || snap.OverlayDrift[0].Field != "model" {
				t.Errorf("drift:sess OverlayDrift = %+v", snap.OverlayDrift)
			}
		case "clean:sess":
			if snap.OverlayDrift == nil || len(snap.OverlayDrift) != 0 {
				t.Errorf("clean:sess OverlayDrift = %#v, want non-nil empty", snap.OverlayDrift)
			}
		}
	}
}

// TestGoldenStateMergeArgvLayers walks the shim golden matrix and asserts
// the spawn-layer merge accepts every archived overlay shape without
// panicking — the "old shim + new main process" reconnect precondition
// (#2543). Fixtures are copied to 0600 temp files because ReadStateFile
// refuses the 0644 permissions git checkouts produce.
func TestGoldenStateMergeArgvLayers(t *testing.T) {
	t.Parallel()
	matches, err := filepath.Glob(filepath.Join("..", "shim", "testdata", "state_*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no shim golden fixtures found")
	}
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), filepath.Base(m))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := shim.ReadStateFile(path)
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		var ov shim.SpawnOverlay
		if st.SpawnOverlay != nil {
			ov = *st.SpawnOverlay
		}
		merged := mergeArgvLayers(
			backendDefaults{Model: "cfg-model", Effort: "low", Args: []string{"--debug"}},
			"profile-model", ov, "", "")
		if merged.Model == "" {
			t.Errorf("%s: merged model empty", st.Key)
		}
	}
}

// TestShimListDrift covers the offline split: tuning-influenced fields come
// back advisory-only, prompt and config-arg growth come back as drift, and a
// legacy (nil-overlay) state stays silent.
func TestShimListDrift(t *testing.T) {
	t.Parallel()
	st := shim.State{
		Backend: "claude",
		CLIArgs: []string{
			"-p", "--model", "old-model", "--append-system-prompt", "旧提示",
			"--resume", "abc-123",
		},
		SpawnOverlay: &shim.SpawnOverlay{AppendSystemPrompt: "新提示"},
	}
	advisory, drift := ShimListDrift("new-model", "", []string{"--debug"}, "", st)

	if len(advisory) != 1 || advisory[0].Field != "model" ||
		advisory[0].Stored != "old-model" || advisory[0].Current != "new-model" {
		t.Errorf("advisory = %+v, want the model note", advisory)
	}
	wantDrift := map[string]bool{"append_system_prompt": false, "extra_args": false}
	for _, d := range drift {
		if _, ok := wantDrift[d.Field]; !ok {
			t.Errorf("unexpected drift field %+v", d)
			continue
		}
		wantDrift[d.Field] = true
	}
	for f, seen := range wantDrift {
		if !seen {
			t.Errorf("missing drift for %s (got %+v)", f, drift)
		}
	}

	if a, d := ShimListDrift("m", "", nil, "", shim.State{CLIArgs: []string{"-p"}}); a != nil || d != nil {
		t.Errorf("nil overlay must stay silent, got %v / %v", a, d)
	}
}
