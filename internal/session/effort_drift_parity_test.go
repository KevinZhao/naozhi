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

	// Every field the drift check must forward, i.e. every field of
	// SpawnOptions that BuildArgs can turn into argv. Extend this when a new
	// argv-bearing field is added.
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
