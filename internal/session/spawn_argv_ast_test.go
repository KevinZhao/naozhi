package session

// spawn_argv_ast_test.go — shared AST helpers for the argv-parity guards.
//
// The guards are source-level rather than behavioural on purpose: dropping a
// field from a SpawnOptions literal still COMPILES and still passes every
// behavioural test, because a struct literal has no type-level obligation to be
// complete. A hand-rolled "parity" test that builds its own two SpawnOptions
// values passes happily while production diverges — see the vacuous pre-fix
// version of TestMCPConfigDriftParity_NoFalsePositive, which compared two
// identical literals and so could never fail.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// argvConstructorFile is the single production site allowed to build a
// cli.SpawnOptions literal for a spawn or drift argv.
const argvConstructorFile = "spawn_argv.go"

// argvOwnerFiles are the paths that consume the constructor and must NOT build
// competing literals of their own.
var argvOwnerFiles = []string{"router_lifecycle.go", "router_shim.go"}

// spawnOptionsLiteralFields returns the field names set by every
// cli.SpawnOptions composite literal in file, plus how many literals it found.
func spawnOptionsLiteralFields(t *testing.T, file string) (fields []string, literals int) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SpawnOptions" {
			return true
		}
		literals++
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Key.(*ast.Ident); ok {
					fields = append(fields, id.Name)
				}
			}
		}
		return false
	})
	return fields, literals
}

// TestSpawnArgv_SingleSourceOfTruth is the structural invariant that lets the
// per-field assertions in this package check ONE literal instead of hunting for
// every construction site.
//
// Before argvSpawnOptions existed, spawnSession and driftCompareArgs each built
// their own cli.SpawnOptions literal and were expected to stay mirrored by
// comment discipline. That failed four times (model/effort, SettingsFile,
// MCPConfigFile, DebugFile), each time silently: a field present on the spawn
// side and absent on the drift side makes every naozhi restart classify every
// live session as arg-drift and kill its CLI.
//
// If a future change reintroduces a literal on either consumer, this test fails
// and points back at the constructor.
func TestSpawnArgv_SingleSourceOfTruth(t *testing.T) {
	t.Parallel()

	if _, n := spawnOptionsLiteralFields(t, argvConstructorFile); n != 1 {
		t.Fatalf("%s holds %d cli.SpawnOptions literals, want exactly 1 — "+
			"the argv constructor must stay the single source of truth",
			argvConstructorFile, n)
	}
	for _, file := range argvOwnerFiles {
		if _, n := spawnOptionsLiteralFields(t, file); n != 0 {
			t.Errorf("%s builds %d cli.SpawnOptions literal(s) of its own — route it "+
				"through argvSpawnOptions (%s) instead, or the two argv paths will "+
				"drift apart again", file, n, argvConstructorFile)
		}
	}
}
