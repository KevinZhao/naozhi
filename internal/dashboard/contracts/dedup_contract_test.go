package contracts

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// dashboardRoot is internal/dashboard relative to this package directory.
const dashboardRoot = ".."

// narrowNodeAccessorAllowed lists the sub-packages permitted to keep a local
// NodeAccessor interface that is a strict subset of contracts.NodeAccessor.
// Their existing test doubles implement only the listed methods; widening
// them would force test churn for no runtime benefit (#2285 trade-off).
var narrowNodeAccessorAllowed = map[string]bool{
	"discovery": true,
	"ext/cli":   true,
}

// TestDashboardContractsDeclaredOnce pins the #2285 dedup: within
// internal/dashboard, `type IPLimiter interface{...}` is declared exactly once
// (here), the full NodeAccessor interface is declared exactly once (here), and
// the helper functions folded into contracts / httputil no longer have local
// copies anywhere in the tree.
func TestDashboardContractsDeclaredOnce(t *testing.T) {
	sharedNA := interfaceMethodSet(t, filepath.Join(".", "contracts.go"), "NodeAccessor")
	if len(sharedNA) == 0 {
		t.Fatal("contracts.NodeAccessor not found or has no methods")
	}

	var ipLimiterDecls, nodeAccessorDecls []string
	var strayFuncs []string
	strayFuncNames := map[string]bool{
		"withMaxBytes":          true,
		"rejectIfTooManyFields": true,
		"isUnknownRPCMethodErr": true,
	}

	err := filepath.WalkDir(dashboardRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dashboardRoot, path)
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		for _, decl := range f.Decls {
			switch dd := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range dd.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					it, isIface := ts.Type.(*ast.InterfaceType)
					if !isIface {
						continue // type aliases (`= contracts.X`) are the intended form
					}
					switch ts.Name.Name {
					case "IPLimiter":
						ipLimiterDecls = append(ipLimiterDecls, rel)
					case "NodeAccessor":
						nodeAccessorDecls = append(nodeAccessorDecls, rel)
						if pkgDir == "contracts" {
							continue
						}
						if !narrowNodeAccessorAllowed[pkgDir] {
							t.Errorf("%s: NodeAccessor interface re-declared; alias contracts.NodeAccessor instead", rel)
						}
						for _, m := range methodNames(t, rel, it) {
							if !sharedNA[m] {
								t.Errorf("%s: narrow NodeAccessor declares %s which is not in contracts.NodeAccessor", rel, m)
							}
						}
					}
				}
			case *ast.FuncDecl:
				if dd.Recv == nil && strayFuncNames[dd.Name.Name] {
					strayFuncs = append(strayFuncs, rel+":"+dd.Name.Name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dashboardRoot, err)
	}

	sort.Strings(ipLimiterDecls)
	if want := []string{"contracts/contracts.go"}; !equalStrings(ipLimiterDecls, want) {
		t.Errorf("IPLimiter interface declarations = %v, want %v", ipLimiterDecls, want)
	}
	if len(strayFuncs) != 0 {
		t.Errorf("local copies of shared helpers still present: %v", strayFuncs)
	}
}

func interfaceMethodSet(t *testing.T, path, name string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != name {
				continue
			}
			it, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				t.Fatalf("%s.%s is not an interface", path, name)
			}
			set := map[string]bool{}
			for _, m := range methodNames(t, path, it) {
				set[m] = true
			}
			return set
		}
	}
	return nil
}

// methodNames returns the explicitly declared method names of it. An embedded
// interface (a field with no Names) is rejected: the subset check below
// compares names syntactically, and an embedding would let methods slip past
// it unseen.
func methodNames(t *testing.T, path string, it *ast.InterfaceType) []string {
	t.Helper()
	var out []string
	for _, f := range it.Methods.List {
		if len(f.Names) == 0 {
			t.Fatalf("%s: embedded interface in NodeAccessor is not supported by the subset check; declare methods explicitly", path)
		}
		for _, n := range f.Names {
			out = append(out, n.Name)
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
