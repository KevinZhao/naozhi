// anchor-keep: single-decision-site rule for workspace resolution; a second copy behaves identically until the priority order drifts between the copies.
package session

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkspaceResolution_SingleSiteContract pins R222-ARCH-12 (#735): the
// workspace decision (opts.Workspace > workspaceOverrides[chatKey] > old
// session workspace > router default) MUST live in exactly one place —
// resolveSpawnParams. Earlier rounds had this logic copy-pasted
// across the spawn / Resume / ResetAndRecreate; centralisation
// happened in R70-ARCH-H2 (extracted into spawnParams) but no contract
// test pinned the invariant, so a future "quick fix" could silently
// reintroduce the duplication.
//
// Every non-test file of the package is searched (the rule follows the code,
// not a file name) for an assignment of `opts.Workspace` to a variable named
// workspace, `=` or `:=`. There must be exactly ONE such site, inside
// (*Router).resolveSpawnParams. The pre-fix shape of #735 (separate
// resolution branches in Resume/ResetAndRecreate) would re-add this
// assignment in two more spots and trip the assertion, wherever those
// functions live.
//
// Assignments to other fields (e.g. `spawnOpts.Workspace = …`) are not
// counted: only the bare local variable is the decision.
func TestWorkspaceResolution_SingleSiteContract(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		files = append(files, f)
	}
	if len(files) < 40 {
		t.Fatalf("parsed %d production files, below the floor of 40: the scan has gone blind", len(files))
	}
	sites := workspaceDecisionSites(fset, files)
	if len(sites) != 1 {
		t.Fatalf("R222-ARCH-12 (#735) contract broken: workspace decision must live "+
			"in exactly one place (resolveSpawnParams). Found %d "+
			"`workspace = opts.Workspace` sites %v; expected 1. If you intentionally "+
			"reintroduced a second workspace resolver, route it through "+
			"resolveSpawnParams or update this test with the new contract.",
			len(sites), sites)
	}
	if sites[0].fn != "(*Router).resolveSpawnParams" {
		t.Errorf("workspace decision moved out of resolveSpawnParams into %s (%s). "+
			"Move it back, or update this contract test if the refactor is intentional.",
			sites[0].fn, sites[0].at)
	}
}

type workspaceSite struct{ at, fn string }

// workspaceDecisionSites returns every `workspace = opts.Workspace`, `:=` or
// `var workspace = opts.Workspace` in files, function literals included, with
// the top-level declaration that holds it ("<package var>" outside any func).
func workspaceDecisionSites(fset *token.FileSet, files []*ast.File) []workspaceSite {
	var sites []workspaceSite
	isDecision := func(lhs *ast.Ident, rhs ast.Expr) bool {
		sel, ok := rhs.(*ast.SelectorExpr)
		if lhs.Name != "workspace" || !ok || sel.Sel.Name != "Workspace" {
			return false
		}
		x, ok := sel.X.(*ast.Ident)
		return ok && x.Name == "opts"
	}
	for _, f := range files {
		for _, d := range f.Decls {
			fn := "<package var>"
			if fd, ok := d.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
				if fd.Recv != nil && len(fd.Recv.List) == 1 {
					recv := recvBase(fd.Recv.List[0].Type)
					if _, star := fd.Recv.List[0].Type.(*ast.StarExpr); star {
						recv = "*" + recv
					}
					fn = "(" + recv + ")." + fn
				}
			}
			add := func(p token.Pos) {
				at := fset.Position(p)
				sites = append(sites, workspaceSite{at: fmt.Sprintf("%s:%d", at.Filename, at.Line), fn: fn})
			}
			ast.Inspect(d, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.AssignStmt:
					if len(x.Lhs) != len(x.Rhs) {
						return true
					}
					for i, lhs := range x.Lhs {
						if id, ok := lhs.(*ast.Ident); ok && isDecision(id, x.Rhs[i]) {
							add(x.Pos())
						}
					}
				case *ast.ValueSpec:
					if len(x.Names) != len(x.Values) {
						return true
					}
					for i, id := range x.Names {
						if isDecision(id, x.Values[i]) {
							add(x.Pos())
						}
					}
				}
				return true
			})
		}
	}
	return sites
}

// The site finder counts `=`, `:=` and `var` to the bare local, in methods,
// plain functions and function literals at package level alike, and nothing
// that only resembles it.
func TestWorkspaceDecisionSites_Fixture(t *testing.T) {
	t.Parallel()
	const src = `package session
func (r *Router) a(opts AgentOpts) { var workspace string; workspace = opts.Workspace; _ = workspace }
func b(opts AgentOpts) { workspace := opts.Workspace; _ = workspace }
func c(opts, other AgentOpts, so *spawnParams) {
	so.workspace = opts.Workspace
	workspace := other.Workspace
	workspace = opts.Model
	_ = workspace
}
func d(opts AgentOpts) { var workspace = opts.Workspace; _ = workspace }
var e = func(opts AgentOpts) string { var workspace string; workspace = opts.Workspace; return workspace }
var g = func(opts AgentOpts) string { var workspace, other = opts.Workspace, opts.Model; _ = other; return workspace }
var h = func(opts AgentOpts) string { var workspace = opts.Model; return workspace }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	got := workspaceDecisionSites(fset, []*ast.File{f})
	want := []workspaceSite{{"fixture.go:2", "(*Router).a"}, {"fixture.go:3", "b"}, {"fixture.go:10", "d"},
		{"fixture.go:11", "<package var>"}, {"fixture.go:12", "<package var>"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("sites = %v, want %v", got, want)
	}
}
