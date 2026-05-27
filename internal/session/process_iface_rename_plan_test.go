package session

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestProcessIfaceGetterRenamePlanned pins the R219-CR-9 (#665) rename
// plan: processIface currently exposes GetSessionID() / GetState(),
// which violate the Go convention of dropping the `Get` prefix on
// accessors. The full rename touches:
//
//   - internal/session/managed.go (this interface)
//   - internal/session/testutil.go (TestProcess fake)
//   - internal/session/router_test.go (fakeProcess fake)
//   - internal/session/router_cleanup_state_cache_test.go (countingProc wrapper)
//   - internal/session/store.go, router_lifecycle.go, router_cleanup.go,
//     managed.go (~5 callsites)
//   - internal/cli/process.go (the only production *Process implementation)
//   - internal/cli/process_turn.go, process_event_query.go (cli-internal callers)
//
// Until the coordinated breaking-change PR lands, the interface keeps
// the unidiomatic names. This test asserts the names are still
// GetState / GetSessionID so a partial / drive-by rename of one half
// (e.g. just adding new aliases without retiring the old methods)
// cannot slip in unobserved — when the full rename ships, this test
// gets flipped (or removed) in the same PR.
func TestProcessIfaceGetterRenamePlanned(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "managed.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse managed.go: %v", err)
	}

	var iface *ast.InterfaceType
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "processIface" {
				continue
			}
			it, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				continue
			}
			iface = it
			break
		}
		if iface != nil {
			break
		}
	}
	if iface == nil {
		t.Fatal("processIface interface not found in managed.go — rename or relocation? " +
			"Update R219-CR-9 (#665) tracker accordingly.")
	}

	want := map[string]bool{
		"GetSessionID": false,
		"GetState":     false,
	}
	for _, m := range iface.Methods.List {
		for _, n := range m.Names {
			if _, ok := want[n.Name]; ok {
				want[n.Name] = true
			}
			// If a renamed counterpart appears alongside, the rename
			// is in flight — flag so the rename PR knows to flip this
			// test as part of the same change.
			if n.Name == "SessionID" || n.Name == "State" {
				t.Errorf("processIface added %q while still keeping the Get-prefixed twin. "+
					"R219-CR-9 (#665) wants a single coordinated rename; either drop "+
					"Get* methods in this PR (and update this test to assert SessionID / "+
					"State are present, GetSessionID / GetState are absent) or do not "+
					"introduce the new names yet.", n.Name)
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("processIface no longer declares %q — was the R219-CR-9 (#665) "+
				"rename completed? If so, flip this test to assert the new names "+
				"and remove this check.", name)
		}
	}
}
