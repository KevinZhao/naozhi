package session

import (
	"go/ast"
	"go/parser"
	"go/token"
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

	// The backend-decidable argv fields — those the drift check CAN know,
	// because backendDefaultsFor resolves them from backend-level config alone.
	//
	// Deliberately not "every argv-bearing field": DebugFile also reaches argv
	// but is a per-session path the drift side cannot reconstruct, so with
	// NAOZHI_CLI_DEBUG enabled every claude shim already reads as drifted.
	// That is a pre-existing gap of the same family as the per-agent overrides
	// documented in §4.5.1, and out of scope here — listing it would assert a
	// mirror that does not and cannot exist today.
	//
	// Extend this when a field is added that backend-level config CAN resolve.
	required := []string{"Model", "ExtraArgs", "Effort", "SettingsFile"}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "router_shim.go", nil, 0)
	if err != nil {
		t.Fatalf("parse router_shim.go: %v", err)
	}

	// Find the cli.SpawnOptions composite literal passed to BuildArgs and
	// collect the field names it sets.
	var got []string
	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "BuildArgs" || len(call.Args) != 1 {
			return true
		}
		lit, ok := call.Args[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		found = true
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Key.(*ast.Ident); ok {
					got = append(got, id.Name)
				}
			}
		}
		return false
	})

	if !found {
		t.Fatal("no BuildArgs(cli.SpawnOptions{...}) call found in router_shim.go — " +
			"if the drift check moved, move this test with it")
	}
	for _, want := range required {
		if !slices.Contains(got, want) {
			t.Errorf("router_shim.go drift check omits SpawnOptions.%s (sets %v).\n"+
				"The real spawn passes it, so every restart would read live shims as "+
				"arg-drift and needlessly restart healthy sessions.", want, got)
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
		r.wsStore.overrides = make(map[string]string)
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

// TestSpawnOptionsLiteral_CarriesEffort is the mirror of the drift-side AST
// assertion, pointed at the real spawn. Deleting `Effort: sp.Effort` from the
// SpawnOptions literal in router_lifecycle.go compiles and passes every
// behavioural test — the tier just silently stops reaching the CLI — so the
// production literal itself has to be asserted.
func TestSpawnOptionsLiteral_CarriesEffort(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "router_lifecycle.go", nil, 0)
	if err != nil {
		t.Fatalf("parse router_lifecycle.go: %v", err)
	}

	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SpawnOptions" {
			return true
		}
		found = true
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Effort" {
					return false
				}
			}
		}
		t.Error("the cli.SpawnOptions literal in router_lifecycle.go does not set " +
			"Effort — the configured tier would never reach the CLI, and no " +
			"behavioural test would notice")
		return false
	})
	if !found {
		t.Fatal("no cli.SpawnOptions literal found in router_lifecycle.go — " +
			"if spawn assembly moved, move this test with it")
	}
}

// TestFirstArgvDivergence covers the drift-log helper that lets an operator
// tell an expected restart (they changed the configured tier) from a spurious
// one (a per-agent override the backend-only drift comparison cannot see).
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

// TestAgentEffortIsSeenAsDrift documents the known limitation head-on rather
// than leaving it to be rediscovered: the drift comparison only sees
// BACKEND-level defaults, so a per-agent tier override reads as drift and the
// surviving shim gets restarted.
//
// This asserts CURRENT behaviour, not desired behaviour. It is pinned so that
// a future fix (persisting the effective argv into shim state, or narrowing the
// comparison to the backend-decidable subset) has to update this test
// deliberately and cannot "accidentally" appear to still hold.
// docs/rfc/kiro-effort-control.md §4.5.1
func TestAgentEffortIsSeenAsDrift(t *testing.T) {
	t.Parallel()
	r := &Router{}
	r.bkStore.model = "claude-fable-5"
	r.bkStore.backendEfforts = map[string]string{"kiro": "high"}
	proto := &cli.ACPProtocol{BackendID: "kiro"}

	bd := r.backendDefaultsFor("kiro")
	driftArgs := proto.BuildArgs(cli.SpawnOptions{
		Model: bd.Model, ExtraArgs: bd.Args, Effort: bd.Effort,
	})
	// What spawnSession would build for a session whose agent overrides the tier.
	spawnArgs := proto.BuildArgs(cli.SpawnOptions{
		Model: bd.Model, ExtraArgs: bd.Args, Effort: "max",
	})

	if slices.Equal(driftArgs, spawnArgs) {
		t.Fatal("agent-level effort no longer diverges from the drift check — " +
			"if this was fixed deliberately, update §4.5.1 and the KNOWN " +
			"LIMITATION comment in router_shim.go")
	}
	// The same holds for the older agents[].model, which is why this is a
	// widened pre-existing gap rather than a defect introduced with effort.
	if slices.Equal(
		proto.BuildArgs(cli.SpawnOptions{Model: "sonnet"}),
		proto.BuildArgs(cli.SpawnOptions{Model: "opus"}),
	) {
		t.Error("expected agents[].model to diverge too — the shared root cause " +
			"cited in §4.5.1 no longer reproduces")
	}
}

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
