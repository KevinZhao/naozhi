package session

// agent_overlay_drift_test.go — regression guard for #2494 (R20260904-REG-2).
//
// Symptom: every naozhi restart killed every live session whose agent set
// agents[].model / agents[].effort / agents[].extra_args. driftCompareArgs
// rebuilt the argv from backend defaults + session tuning only, while the real
// spawn (resolveSpawnParamsLocked) layered the per-agent AgentOpts on top —
// so `--model sonnet` (spawned) never equalled `--model opusplan` (rebuilt),
// classifyShimState saw shimStateDrift, and the shim was shut down.
//
// Fix: the spawn persists its per-request layer (shim.SpawnOverlay) into the
// shim state; the drift side re-merges that overlay with CURRENT backend
// defaults through the same mergeArgvLayers. These tests drive BOTH production
// paths end-to-end on a shim.State value — no hand-built argv on either side —
// so they cannot pass by both sides being equally wrong (the trap the older
// parity files document).
//
// Verified failing against origin/master (49bffb15) with the same scenario:
//
//	real spawn: [... --dangerously-skip-permissions --model sonnet]
//	drift:      [... --dangerously-skip-permissions --model opusplan]

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/shim"
)

// mkOverlayRouter builds a two-backend router (claude default model
// "opusplan", kiro effort "high") mirroring the config shape in #2494.
func mkOverlayRouter(t *testing.T) *Router {
	t.Helper()
	r := &Router{
		ss:         sessionStore{sessions: make(map[string]*ManagedSession)},
		defaultCWD: "/default/ws",
	}
	r.bkStore.wrappers = map[string]*cli.Wrapper{
		"claude": cli.NewWrapperLazy("/bin/false", &cli.ClaudeProtocol{}, "claude"),
		"kiro":   cli.NewWrapper("/bin/false", &cli.ACPProtocol{BackendID: "kiro"}, "kiro"),
	}
	r.bkStore.defaultBackend = "claude"
	r.bkStore.backendOverrides = make(map[string]string)
	r.bkStore.accessProfileOverrides = make(map[string]string)
	r.bkStore.backendEfforts = map[string]string{"kiro": "high"}
	r.bkStore.model = "opusplan"
	r.claudeDir = t.TempDir()
	r.kiroSessionsDir = t.TempDir()
	return r
}

// spawnShimState runs the PRODUCTION spawn-side computation for key/opts and
// returns the shim.State a shim spawned from it would persist: the argv
// exactly as spawnSession assembles it (resolveSpawnParamsLocked →
// argvSpawnOptions → BuildArgs, plus --resume) and the overlay spawnSession
// hands to the shim.
func spawnShimState(t *testing.T, r *Router, key, resumeID string, opts AgentOpts) (shim.State, spawnParams) {
	t.Helper()
	sp := r.resolveSpawnParamsLocked(key, resumeID, opts)
	if sp.Wrapper == nil {
		t.Fatalf("no wrapper for backend %q", sp.BackendID)
	}
	spawnOpts := r.argvSpawnOptions(sp.Model, sp.Effort, r.cliDebugFileFor(key), sp.Args)
	spawnOpts.ResumeID = sp.ResumeID
	overlay := sp.Overlay
	spawnOpts.SpawnOverlay = &overlay
	return shim.State{
		Key:          key,
		Backend:      sp.BackendID,
		Workspace:    sp.Workspace,
		CLIArgs:      sp.Wrapper.Protocol.BuildArgs(spawnOpts),
		SpawnOverlay: spawnOpts.SpawnOverlay,
	}, sp
}

// TestAgentOverlayDrift_ModelOverrideIsNotDrift is the load-bearing #2494
// assertion: agents[].model differs from the backend default, and the surviving
// shim must NOT read as drift on restart.
func TestAgentOverlayDrift_ModelOverrideIsNotDrift(t *testing.T) {
	r := mkOverlayRouter(t)
	key := "dashboard:direct:2494-model:code-reviewer"
	s := newSessionWithID(key, "sess-2494-model")
	s.SetBackend("claude")
	r.ss.sessions[key] = s

	state, _ := spawnShimState(t, r, key, "", AgentOpts{Backend: "claude", Workspace: "/ws", Model: "sonnet"})
	if !slices.Contains(state.CLIArgs, "sonnet") {
		t.Fatalf("agent model never reached argv %v — assertion below would be vacuous", state.CLIArgs)
	}

	wrapper, backendID := r.wrapperFor(state.Backend)
	drift, stored, current := r.shimArgsDrift(wrapper, backendID, state, s)
	if drift {
		t.Fatalf("agents[].model session misread as drift — every naozhi restart would kill it\n"+
			"  stored:  %v\n  current: %v", stored, current)
	}
}

// TestAgentOverlayDrift_EffortAndExtraArgsAreNotDrift covers the other two
// per-request tiers on the ACP backend (the only one that renders --effort).
func TestAgentOverlayDrift_EffortAndExtraArgsAreNotDrift(t *testing.T) {
	r := mkOverlayRouter(t)
	key := "dashboard:direct:2494-effort:reviewer"
	s := newSessionWithID(key, "sess-2494-effort")
	s.SetBackend("kiro")
	r.ss.sessions[key] = s

	state, _ := spawnShimState(t, r, key, "", AgentOpts{
		Backend: "kiro", Workspace: "/ws", Effort: "max", ExtraArgs: []string{"--agent-flag", "v1"},
	})
	if !slices.Contains(state.CLIArgs, "max") || !slices.Contains(state.CLIArgs, "--agent-flag") {
		t.Fatalf("agent effort/extra args never reached argv %v", state.CLIArgs)
	}

	wrapper, backendID := r.wrapperFor(state.Backend)
	if drift, stored, current := r.shimArgsDrift(wrapper, backendID, state, s); drift {
		t.Fatalf("agents[].effort/extra_args session misread as drift\n  stored:  %v\n  current: %v", stored, current)
	}
}

// TestAgentOverlayDrift_BackendConfigChangeIsStillDrift pins the other half of
// the contract: the overlay must not mask a GENUINE config change. The operator
// edits the backend default (cli.model / cli.backends[].extra_args) while the
// agent-override session is alive; the restart must apply it.
func TestAgentOverlayDrift_BackendConfigChangeIsStillDrift(t *testing.T) {
	t.Run("backend model change under an agent effort override", func(t *testing.T) {
		r := mkOverlayRouter(t)
		key := "dashboard:direct:2494-cfg1:reviewer"
		s := newSessionWithID(key, "sess-2494-cfg1")
		s.SetBackend("kiro")
		r.ss.sessions[key] = s
		state, _ := spawnShimState(t, r, key, "", AgentOpts{Backend: "kiro", Workspace: "/ws", Effort: "max"})

		r.bkStore.model = "claude-haiku-4.5" // operator edits cli.model, restarts naozhi

		wrapper, backendID := r.wrapperFor(state.Backend)
		drift, stored, current := r.shimArgsDrift(wrapper, backendID, state, s)
		if !drift {
			t.Fatalf("backend model change masked by overlay — the restart that applies it would never trigger\n"+
				"  stored:  %v\n  current: %v", stored, current)
		}
		if !slices.Contains(current, "max") {
			t.Errorf("rebuilt argv lost the agent effort override: %v", current)
		}
	})

	t.Run("backend extra_args change under an agent model override", func(t *testing.T) {
		r := mkOverlayRouter(t)
		key := "dashboard:direct:2494-cfg2:code-reviewer"
		s := newSessionWithID(key, "sess-2494-cfg2")
		s.SetBackend("claude")
		r.ss.sessions[key] = s
		state, _ := spawnShimState(t, r, key, "", AgentOpts{Backend: "claude", Workspace: "/ws", Model: "sonnet"})

		r.bkStore.backendExtraArgs = map[string][]string{"claude": {"--max-turns", "50"}}

		wrapper, backendID := r.wrapperFor(state.Backend)
		drift, _, current := r.shimArgsDrift(wrapper, backendID, state, s)
		if !drift {
			t.Fatalf("backend extra_args change masked by overlay: %v", current)
		}
		if !slices.Contains(current, "sonnet") {
			t.Errorf("rebuilt argv lost the agent model override: %v", current)
		}
	})
}

// TestAgentOverlayDrift_AccessProfileDefaultModel covers the profile tier:
// the overlay carries the resolved profile ID, and the drift side resolves the
// profile's default_model from CURRENT config — so an unchanged profile is not
// drift, and an edited profile default_model is.
func TestAgentOverlayDrift_AccessProfileDefaultModel(t *testing.T) {
	r := mkOverlayRouter(t)
	r.accessProfiles = map[string]AccessProfile{
		"work": {DefaultModel: "claude-sonnet-4.6", Env: map[string]string{"X": "1"}},
	}
	key := "dashboard:direct:2494-profile:general"
	s := newSessionWithID(key, "sess-2494-profile")
	s.SetBackend("claude")
	r.ss.sessions[key] = s

	state, sp := spawnShimState(t, r, key, "", AgentOpts{Backend: "claude", Workspace: "/ws", AccessProfile: "work"})
	if sp.Overlay.AccessProfile != "work" || sp.Overlay.Model != "" {
		t.Fatalf("overlay = %+v, want AccessProfile=work and no explicit Model", sp.Overlay)
	}
	if !slices.Contains(state.CLIArgs, "claude-sonnet-4.6") {
		t.Fatalf("profile default_model never reached argv %v", state.CLIArgs)
	}

	wrapper, backendID := r.wrapperFor(state.Backend)
	if drift, stored, current := r.shimArgsDrift(wrapper, backendID, state, s); drift {
		t.Fatalf("access-profile session misread as drift\n  stored:  %v\n  current: %v", stored, current)
	}

	// Operator edits the profile's default_model → genuine drift.
	r.accessProfiles = map[string]AccessProfile{"work": {DefaultModel: "claude-haiku-4.5"}}
	if drift, _, current := r.shimArgsDrift(wrapper, backendID, state, s); !drift {
		t.Fatalf("profile default_model change masked: %v", current)
	}
}

// TestAgentOverlayDrift_TuningStaysOnTop: a dashboard tuning pick outranks the
// agent override on BOTH sides, and the persisted overlay must not resurrect
// the agent model above it.
func TestAgentOverlayDrift_TuningStaysOnTop(t *testing.T) {
	r := mkOverlayRouter(t)
	key := "dashboard:direct:2494-tuning:code-reviewer"
	s := newSessionWithID(key, "sess-2494-tuning")
	s.SetBackend("claude")
	s.SetTuningModel("claude-haiku-4.5")
	r.ss.sessions[key] = s

	state, _ := spawnShimState(t, r, key, "", AgentOpts{Backend: "claude", Workspace: "/ws", Model: "sonnet"})
	if !slices.Contains(state.CLIArgs, "claude-haiku-4.5") || slices.Contains(state.CLIArgs, "sonnet") {
		t.Fatalf("tuning did not outrank the agent model in the spawn argv: %v", state.CLIArgs)
	}
	wrapper, backendID := r.wrapperFor(state.Backend)
	if drift, stored, current := r.shimArgsDrift(wrapper, backendID, state, s); drift {
		t.Fatalf("tuned + agent-override session misread as drift\n  stored:  %v\n  current: %v", stored, current)
	}
	// Tuning change is still real drift (unchanged §4.5 semantics).
	s.SetTuningModel("claude-sonnet-4.6")
	if drift, _, _ := r.shimArgsDrift(wrapper, backendID, state, s); !drift {
		t.Fatal("tuning change no longer surfaces as drift")
	}
}

// TestAgentOverlayDrift_ResumeArgsStripped: a resumed spawn records --resume
// <id> in CLIArgs; the comparison must keep stripping it (unchanged behaviour,
// pinned because the overlay path now feeds the same predicate).
func TestAgentOverlayDrift_ResumeArgsStripped(t *testing.T) {
	r := mkOverlayRouter(t)
	key := "dashboard:direct:2494-resume:code-reviewer"
	s := newSessionWithID(key, "sess-2494-resume")
	s.SetBackend("claude")
	r.ss.sessions[key] = s

	state, sp := spawnShimState(t, r, key, "", AgentOpts{Backend: "claude", Workspace: "/ws", Model: "sonnet"})
	// resolveResumeID downgrades a missing on-disk target to fresh; inject the
	// pair the way a real resume would have recorded it.
	_ = sp
	state.CLIArgs = append(slices.Clone(state.CLIArgs), "--resume", "sess-2494-resume")

	wrapper, backendID := r.wrapperFor(state.Backend)
	drift, stored, _ := r.shimArgsDrift(wrapper, backendID, state, s)
	if drift {
		t.Fatalf("resumed agent-override session misread as drift; stored=%v", stored)
	}
	if slices.Contains(stored, "--resume") {
		t.Errorf("stored base still carries --resume: %v", stored)
	}
}

// TestAgentOverlayDrift_LegacyStateFallsBack pins the upgrade-window behaviour
// for a shim spawned BEFORE the overlay existed (SpawnOverlay == nil in its
// state file):
//
//   - it must not panic and must not be blanket-killed or blanket-kept: the
//     comparison degrades to the pre-#2494 backend-defaults rebuild, so a
//     no-override session reconnects and an agent-override session drifts
//     exactly once (its replacement shim then carries the overlay);
//   - the fallback is logged so the operator can attribute that one restart.
func TestAgentOverlayDrift_LegacyStateFallsBack(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	r := mkOverlayRouter(t)
	wrapper, backendID := r.wrapperFor("claude")

	t.Run("no-override legacy shim reconnects", func(t *testing.T) {
		key := "dashboard:direct:2494-legacy-plain:general"
		s := newSessionWithID(key, "sess-legacy-plain")
		s.SetBackend("claude")
		r.ss.sessions[key] = s
		state, _ := spawnShimState(t, r, key, "", AgentOpts{Backend: "claude", Workspace: "/ws"})
		state.SpawnOverlay = nil // written by a pre-#2494 shim

		if drift, stored, current := r.shimArgsDrift(wrapper, backendID, state, s); drift {
			t.Fatalf("legacy no-override shim misread as drift\n  stored:  %v\n  current: %v", stored, current)
		}
	})

	t.Run("agent-override legacy shim drifts once, with breadcrumb", func(t *testing.T) {
		logs.Reset()
		key := "dashboard:direct:2494-legacy-agent:code-reviewer"
		s := newSessionWithID(key, "sess-legacy-agent")
		s.SetBackend("claude")
		r.ss.sessions[key] = s
		state, _ := spawnShimState(t, r, key, "", AgentOpts{Backend: "claude", Workspace: "/ws", Model: "sonnet"})
		state.SpawnOverlay = nil

		drift, _, _ := r.shimArgsDrift(wrapper, backendID, state, s)
		if !drift {
			t.Fatal("legacy agent-override shim: expected the documented one-time drift (pre-fix behaviour)")
		}
		if !strings.Contains(logs.String(), "predates spawn-overlay persistence") {
			t.Errorf("legacy fallback not logged; got:\n%s", logs.String())
		}
	})

	t.Run("nil session (adopt path) with legacy state does not panic", func(t *testing.T) {
		state := shim.State{Key: "dashboard:direct:2494-legacy-adopt:general", Backend: "claude",
			CLIArgs: []string{"-p", "--model", "opusplan"}}
		_, _, _ = r.shimArgsDrift(wrapper, backendID, state, nil)
	})

	t.Run("empty stored argv is never drift and logs nothing", func(t *testing.T) {
		logs.Reset()
		state := shim.State{Key: "dashboard:direct:2494-legacy-empty:general", Backend: "claude"}
		if drift, _, _ := r.shimArgsDrift(wrapper, backendID, state, nil); drift {
			t.Fatal("empty CLIArgs must not read as drift")
		}
		if logs.Len() != 0 {
			t.Errorf("unexpected log for empty argv: %s", logs.String())
		}
	})
}

// TestAgentOverlayDrift_KnownEmptyOverlayIsNotLegacy: a shim spawned by this
// version with NO overrides records `{}` (non-nil, zero value) — it must be
// treated as fully known, not as legacy, so no fallback breadcrumb fires.
func TestAgentOverlayDrift_KnownEmptyOverlayIsNotLegacy(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	r := mkOverlayRouter(t)
	key := "dashboard:direct:2494-empty:general"
	s := newSessionWithID(key, "sess-2494-empty")
	s.SetBackend("claude")
	r.ss.sessions[key] = s
	state, sp := spawnShimState(t, r, key, "", AgentOpts{Backend: "claude", Workspace: "/ws"})
	if ov := sp.Overlay; ov.Model != "" || ov.Effort != "" || ov.AccessProfile != "" || len(ov.ExtraArgs) != 0 {
		t.Fatalf("no-override spawn produced a populated overlay: %+v", ov)
	}
	if state.SpawnOverlay == nil {
		t.Fatal("spawn path must always hand the shim a non-nil overlay")
	}
	wrapper, backendID := r.wrapperFor(state.Backend)
	if drift, _, _ := r.shimArgsDrift(wrapper, backendID, state, s); drift {
		t.Fatal("no-override session misread as drift")
	}
	if strings.Contains(logs.String(), "predates spawn-overlay") {
		t.Errorf("known-empty overlay logged as legacy:\n%s", logs.String())
	}
}

// TestAgentOverlayDrift_CompareHasNoSpawnSideEffects: resolveSpawnParamsLocked
// CONSUMES the one-shot dashboard picks (backendOverrides /
// accessProfileOverrides). The drift comparison runs for every surviving shim
// at startup and must never touch them — otherwise a restart would silently
// eat a pending "pick backend/profile" for the next spawn.
func TestAgentOverlayDrift_CompareHasNoSpawnSideEffects(t *testing.T) {
	r := mkOverlayRouter(t)
	key := "dashboard:direct:2494-sidefx:general"
	r.bkStore.backendOverrides[key] = "kiro"
	r.bkStore.accessProfileOverrides[key] = "work"
	r.accessProfiles = map[string]AccessProfile{"work": {DefaultModel: "m"}}

	wrapper, backendID := r.wrapperFor("claude")
	state := shim.State{Key: key, Backend: "claude", CLIArgs: []string{"-p", "--model", "opusplan"},
		SpawnOverlay: &shim.SpawnOverlay{Model: "sonnet", AccessProfile: "work"}}
	_, _, _ = r.shimArgsDrift(wrapper, backendID, state, nil)

	if got := r.bkStore.backendOverrides[key]; got != "kiro" {
		t.Errorf("drift compare consumed backendOverrides[%s]: got %q", key, got)
	}
	if got := r.bkStore.accessProfileOverrides[key]; got != "work" {
		t.Errorf("drift compare consumed accessProfileOverrides[%s]: got %q", key, got)
	}
}

// TestMergeArgvLayers pins the precedence table the two callers share, and
// that the returned Args never alias the backend's configured slice.
func TestMergeArgvLayers(t *testing.T) {
	t.Parallel()
	bd := backendDefaults{Model: "base", Effort: "high", Args: []string{"--a"}}

	cases := []struct {
		name          string
		profileModel  string
		ov            shim.SpawnOverlay
		tuningM, tunE string
		wantM, wantE  string
		wantArgs      []string
	}{
		{"backend only", "", shim.SpawnOverlay{}, "", "", "base", "high", []string{"--a"}},
		{"profile above backend", "prof", shim.SpawnOverlay{}, "", "", "prof", "high", []string{"--a"}},
		{"agent above profile", "prof", shim.SpawnOverlay{Model: "agent", Effort: "max", ExtraArgs: []string{"--b"}}, "", "", "agent", "max", []string{"--a", "--b"}},
		{"tuning above agent", "prof", shim.SpawnOverlay{Model: "agent", Effort: "max"}, "tuneM", "low", "tuneM", "low", []string{"--a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeArgvLayers(bd, tc.profileModel, tc.ov, tc.tuningM, tc.tunE)
			if got.Model != tc.wantM || got.Effort != tc.wantE || !slices.Equal(got.Args, tc.wantArgs) {
				t.Errorf("mergeArgvLayers = %+v, want model=%q effort=%q args=%v", got, tc.wantM, tc.wantE, tc.wantArgs)
			}
		})
	}

	got := mergeArgvLayers(bd, "", shim.SpawnOverlay{ExtraArgs: []string{"--b"}}, "", "")
	got.Args[0] = "mutated"
	if bd.Args[0] != "--a" {
		t.Error("mergeArgvLayers aliased the backend's configured Args slice")
	}
}

// TestResolveSpawnParams_RecordsOverlay: the overlay on spawnParams is exactly
// the per-request layer (opts fields + RESOLVED profile), so what the shim
// persists is what the drift side needs — including the profile fallback to
// "" when the configured id is unknown (mirrors the spawn's own fallback).
func TestResolveSpawnParams_RecordsOverlay(t *testing.T) {
	r := mkOverlayRouter(t)
	r.accessProfiles = map[string]AccessProfile{"work": {DefaultModel: "m"}}

	sp := r.resolveSpawnParamsLocked("dashboard:direct:2494-rec:reviewer", "", AgentOpts{
		Backend: "kiro", Workspace: "/ws", Model: "sonnet", Effort: "max",
		ExtraArgs: []string{"--x"}, AccessProfile: "work",
	})
	want := shim.SpawnOverlay{Model: "sonnet", Effort: "max", ExtraArgs: []string{"--x"}, AccessProfile: "work"}
	if sp.Overlay.Model != want.Model || sp.Overlay.Effort != want.Effort ||
		sp.Overlay.AccessProfile != want.AccessProfile || !slices.Equal(sp.Overlay.ExtraArgs, want.ExtraArgs) {
		t.Errorf("Overlay = %+v, want %+v", sp.Overlay, want)
	}

	sp = r.resolveSpawnParamsLocked("dashboard:direct:2494-rec2:general", "", AgentOpts{
		Backend: "claude", Workspace: "/ws", AccessProfile: "deleted-profile",
	})
	if sp.Overlay.AccessProfile != "" || sp.AccessProfileID != "" {
		t.Errorf("unknown profile must resolve to \"\" on both spawnParams and overlay; got %q / %q",
			sp.AccessProfileID, sp.Overlay.AccessProfile)
	}
}
