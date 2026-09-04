package session

import (
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestEffortDriftCheck_MirrorsSpawn guards the highest-consequence way this
// feature can break.
//
// Two independent places feed cli.SpawnOptions into Protocol.BuildArgs:
//
//	spawnSession                     — the real spawn (router_lifecycle.go)
//	classifyShimState's drift check  — "do the surviving shim's args still
//	                                   match what we would spawn today?"
//	                                   (router_shim.go)
//
// If the drift check omits a field the real spawn passes, the two argv lists
// differ on every restart, every live kiro shim is classified as
// shimStateDrift, and healthy sessions get restarted — the operator sees
// "restarting naozhi loses all my kiro sessions". SettingsFile hit exactly this
// trap before (see the comment at its mirror site), and Effort is now in the
// same position.
//
// The check is a source-level assertion rather than a behavioural one on
// purpose: dropping the field still COMPILES and still passes every
// behavioural test, because both call sites are ordinary struct literals with
// no shared type forcing them to agree. Verified by deleting the field — a
// hand-rolled parity test that builds its own two SpawnOptions values passes
// happily, because it never reads the production literal.
// docs/rfc/kiro-effort-control.md §4.5
func TestEffortDriftCheck_MirrorsSpawn(t *testing.T) {
	t.Parallel()

	// Every argv-bearing field the drift check must reproduce. Both paths now
	// build their SpawnOptions in argvSpawnOptions (spawn_argv.go), so this list
	// is asserted against that one constructor — TestSpawnArgv_SingleSourceOfTruth
	// pins that no consumer builds a competing literal.
	//
	// DebugFile is on the list as of the 2026-09-02 fix. It was previously
	// excluded here as a per-session path "the drift side cannot reconstruct" —
	// that was wrong, and the exclusion documented a live bug rather than a
	// limitation: cliDebugPathFor is a pure function of the session key
	// (KeyHash), and the drift loop holds state.Key. While it was omitted, every
	// naozhi restart on a host with NAOZHI_CLI_DEBUG set read EVERY live claude
	// shim as drifted and killed its CLI. See debugfile_drift_parity_test.go.
	//
	// MCPConfigFile qualifies for the same reason SettingsFile does: it is a
	// single router-global value (RouterConfig.MCPConfigFile), not a per-session
	// or per-agent one, so the drift side can reconstruct it exactly.
	// RFC cli-mcp-config G4.
	//
	// AppendSystemPrompt (#2493) is per-session (agent / planner / scratch);
	// both paths obtain it from mergeArgvLayers — the spawn from the overlay it
	// builds, the drift side from shim.SpawnOverlay.AppendSystemPrompt the
	// shim persisted. Listing it here pins that it stays in the ONE literal
	// rather than being spelled out at a call site.
	//
	// Still deliberately absent: ResumeID (session state, stripped from the
	// stored argv by stripResumeArgs) and PermissionMode (both paths rely on the
	// same zero value — see argvSpawnOptions).
	required := []string{"Model", "ExtraArgs", "Effort", "SettingsFile", "MCPConfigFile", "DebugFile", "AppendSystemPrompt"}

	got, literals := spawnOptionsLiteralFields(t, argvConstructorFile)
	if literals == 0 {
		t.Fatalf("no cli.SpawnOptions literal found in %s — if the argv "+
			"constructor moved, move this test with it", argvConstructorFile)
	}
	for _, want := range required {
		if !slices.Contains(got, want) {
			t.Errorf("argvSpawnOptions omits SpawnOptions.%s (sets %v).\n"+
				"Both the real spawn and the drift check read it from here, so every "+
				"restart would read live shims as arg-drift and kill healthy sessions.",
				want, got)
		}
	}
}

// TestEffortAffectsArgv pins the premise the test above rests on: Effort must
// actually change the argv for an ACP backend. If it stopped doing so, the
// mirror in router_shim.go would be dead weight and its comment misleading.
func TestEffortAffectsArgv(t *testing.T) {
	t.Parallel()
	proto := &cli.ACPProtocol{BackendID: "kiro"}
	withTier := proto.BuildArgs(cli.SpawnOptions{Model: "claude-fable-5", Effort: "xhigh"})
	withoutTier := proto.BuildArgs(cli.SpawnOptions{Model: "claude-fable-5"})

	if slices.Equal(withTier, withoutTier) {
		t.Fatal("ACP BuildArgs ignores Effort — the tier no longer reaches kiro, " +
			"and the drift-check mirror in router_shim.go is now pointless")
	}
	if !slices.Contains(withTier, "--effort") {
		t.Errorf("expected --effort in argv, got %v", withTier)
	}
}

// TestResolveSpawnParams_EffortPrecedence covers the MAIN chain: does a
// configured tier actually become a spawn parameter?
//
// This exists because the first cut of these tests guarded the wrong thing.
// Mutation-testing the implementation found that deleting the agent-override
// branch in resolveSpawnParamsLocked, or `Effort: sp.Effort` from the
// SpawnOptions literal, left the entire suite green — every "effort works"
// test built its own SpawnOptions or called backendDefaultsFor directly, so
// nothing asserted the resolver's output. The elaborate AST guard sat on the
// drift mirror while the load-bearing path had none.
// docs/rfc/kiro-effort-control.md §4.2
func TestResolveSpawnParams_EffortPrecedence(t *testing.T) {
	mkRouter := func(t *testing.T, backendEfforts map[string]string) *Router {
		t.Helper()
		r := &Router{
			ss:         sessionStore{sessions: make(map[string]*ManagedSession)},
			defaultCWD: "/default/ws",
		}
		r.bkStore.wrappers = map[string]*cli.Wrapper{
			"kiro":   cli.NewWrapper("/bin/false", &cli.ACPProtocol{BackendID: "kiro"}, "kiro"),
			"claude": cli.NewWrapper("/bin/false", &cli.ClaudeProtocol{}, "claude"),
		}
		r.bkStore.defaultBackend = "kiro"
		r.bkStore.backendOverrides = make(map[string]string)
		r.bkStore.backendEfforts = backendEfforts
		r.claudeDir = t.TempDir()
		r.kiroSessionsDir = t.TempDir()
		return r
	}

	t.Run("backend tier applies when the agent sets none", func(t *testing.T) {
		r := mkRouter(t, map[string]string{"kiro": "high"})
		sp := r.resolveSpawnParamsLocked("dash:direct:c1:general", "",
			AgentOpts{Backend: "kiro", Workspace: "/ws"})
		if sp.Effort != "high" {
			t.Errorf("Effort = %q, want high (backend default)", sp.Effort)
		}
	})

	t.Run("agent tier overrides the backend tier", func(t *testing.T) {
		r := mkRouter(t, map[string]string{"kiro": "high"})
		sp := r.resolveSpawnParamsLocked("dash:direct:c2:reviewer", "",
			AgentOpts{Backend: "kiro", Workspace: "/ws", Effort: "max"})
		if sp.Effort != "max" {
			t.Errorf("Effort = %q, want max (agents[].effort wins)", sp.Effort)
		}
	})

	t.Run("agent tier applies with no backend tier configured", func(t *testing.T) {
		r := mkRouter(t, nil)
		sp := r.resolveSpawnParamsLocked("dash:direct:c3:reviewer", "",
			AgentOpts{Backend: "kiro", Workspace: "/ws", Effort: "low"})
		if sp.Effort != "low" {
			t.Errorf("Effort = %q, want low", sp.Effort)
		}
	})

	t.Run("nothing configured yields no tier", func(t *testing.T) {
		r := mkRouter(t, nil)
		sp := r.resolveSpawnParamsLocked("dash:direct:c4:general", "",
			AgentOpts{Backend: "kiro", Workspace: "/ws"})
		if sp.Effort != "" {
			t.Errorf("Effort = %q, want empty so BuildArgs emits no flag", sp.Effort)
		}
	})

	t.Run("an unconfigured backend gets no tier from another backend", func(t *testing.T) {
		r := mkRouter(t, map[string]string{"kiro": "max"})
		sp := r.resolveSpawnParamsLocked("dash:direct:c5:general", "",
			AgentOpts{Backend: "claude", Workspace: "/ws"})
		if sp.Effort != "" {
			t.Errorf("Effort = %q, want empty — kiro's tier must not leak to claude", sp.Effort)
		}
	})
}

// TestSpawnArgvConstructor_CarriesEffort is the narrow, RFC-owned assertion that
// the tier still reaches argv at all. Deleting `Effort: effort` from
// argvSpawnOptions compiles and passes every behavioural test — the tier just
// silently stops reaching the CLI — so the production literal itself has to be
// asserted. Kept separate from the required-list check above so a
// kiro-effort-control regression names itself in the failure output.
func TestSpawnArgvConstructor_CarriesEffort(t *testing.T) {
	t.Parallel()
	got, literals := spawnOptionsLiteralFields(t, argvConstructorFile)
	if literals == 0 {
		t.Fatalf("no cli.SpawnOptions literal found in %s — if the argv "+
			"constructor moved, move this test with it", argvConstructorFile)
	}
	if !slices.Contains(got, "Effort") {
		t.Errorf("argvSpawnOptions does not set Effort (sets %v) — the configured "+
			"tier would never reach the CLI, and no behavioural test would notice", got)
	}
}

// TestFirstArgvDivergence covers the drift-log helper that lets an operator
// tell an expected restart (they changed the configured tier) from a spurious
// one (a pre-#2494 shim whose state carries no spawn overlay).
func TestFirstArgvDivergence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		old, new         []string
		wantOld, wantNew string
	}{{
		name: "tier changed — the case this was added for",
		old:  []string{"acp", "--model", "m", "--effort", "high"},
		new:  []string{"acp", "--model", "m", "--effort", "max"},
		// Reports the values, not the flag: the flag matched, so the value is
		// the actual divergence and the more useful thing to print.
		wantOld: "high", wantNew: "max",
	}, {
		name:    "flag appeared",
		old:     []string{"acp", "--model", "m"},
		new:     []string{"acp", "--model", "m", "--effort", "max"},
		wantOld: "(absent)", wantNew: "--effort",
	}, {
		name:    "flag disappeared",
		old:     []string{"acp", "--effort", "low"},
		new:     []string{"acp"},
		wantOld: "--effort", wantNew: "(absent)",
	}, {
		name:    "identical yields empty pair",
		old:     []string{"acp", "--model", "m"},
		new:     []string{"acp", "--model", "m"},
		wantOld: "", wantNew: "",
	}, {
		name:    "both empty",
		old:     nil,
		new:     nil,
		wantOld: "", wantNew: "",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotOld, gotNew := firstArgvDivergence(tc.old, tc.new)
			if gotOld != tc.wantOld || gotNew != tc.wantNew {
				t.Errorf("firstArgvDivergence() = (%q, %q), want (%q, %q)",
					gotOld, gotNew, tc.wantOld, tc.wantNew)
			}
		})
	}
}

// TestAgentEffortIsSeenAsDrift used to live here, pinning the §4.5.1 KNOWN
// LIMITATION (a per-agent tier read as drift on every restart). Fixed by
// #2494 — the per-request overlay is persisted in shim state and re-merged on
// reconnect; the inverse assertion now lives in agent_overlay_drift_test.go.

// TestBackendEffortsFeedDriftCheck closes the loop on the router side: the
// drift check reads its tier from backendDefaultsFor, so a configured tier has
// to survive that lookup for the mirror to have anything to pass.
func TestBackendEffortsFeedDriftCheck(t *testing.T) {
	t.Parallel()
	r := &Router{}
	r.bkStore.model = "claude-fable-5"
	r.bkStore.backendEfforts = map[string]string{"kiro": "xhigh"}

	bd := r.backendDefaultsFor("kiro")
	if bd.Effort != "xhigh" {
		t.Fatalf("backendDefaultsFor(kiro).Effort = %q, want xhigh", bd.Effort)
	}
	args := (&cli.ACPProtocol{BackendID: "kiro"}).BuildArgs(cli.SpawnOptions{
		Model: bd.Model, ExtraArgs: bd.Args, Effort: bd.Effort,
	})
	if !slices.Contains(args, "xhigh") {
		t.Errorf("configured tier did not reach argv: %v", args)
	}
}
