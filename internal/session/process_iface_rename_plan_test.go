package session

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestProcessIfaceGetterRenamePlanned pins the R219-CR-9 (#665) rename,
// completed under ADR-0001 PR-2 (#463): processIface now exposes the
// idiomatic SessionID() / State() accessors (the `Get` prefix was
// dropped per the Go convention). The coordinated rename touched:
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
// This test now asserts the new names are present and the old
// Get-prefixed twins are gone, so a regression that re-introduces a
// Get* accessor on processIface cannot slip in unobserved.
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
		"SessionID": false,
		"State":     false,
	}
	for _, m := range iface.Methods.List {
		for _, n := range m.Names {
			if _, ok := want[n.Name]; ok {
				want[n.Name] = true
			}
			// The Get-prefixed twins were retired by the ADR-0001 PR-2
			// (#463) rename. Their reappearance is a regression toward
			// the unidiomatic naming this guard exists to prevent.
			if n.Name == "GetSessionID" || n.Name == "GetState" {
				t.Errorf("processIface re-introduced %q — the ADR-0001 PR-2 (#463) "+
					"rename dropped the Get prefix (SessionID / State). Keep the "+
					"idiomatic accessor names; do not re-add Get* twins.", n.Name)
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("processIface no longer declares %q — the ADR-0001 PR-2 (#463) "+
				"rename established SessionID / State as the canonical accessor "+
				"names; this guard expects them present.", name)
		}
	}
}
