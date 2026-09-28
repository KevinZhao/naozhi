package testhelper

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// goFileName matches a string that names a Go source file or path to one.
var goFileName = regexp.MustCompile(`^[\w./-]*\w\.go$`)

// readsGoSource reports whether a test file's source reads repository Go
// source: it reads from disk (os.ReadFile, os.Open, parser.ParseFile with a nil
// src, parser.ParseDir) and names Go source somewhere — a "x.go" literal,
// however the path is then built, or a "*.go" glob or ".go" suffix scan. It is
// an AST check, so neither the spelling of the path nor a detour through a
// variable hides a read. A .go literal the same file also passes to a write
// (os.WriteFile, os.Create, a helper whose name contains "write") is a fixture
// the test creates, not source it reads.
func readsGoSource(src []byte) bool {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.SkipObjectResolution)
	if err != nil {
		return false
	}
	written := map[string]bool{}
	var calls []*ast.CallExpr
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			calls = append(calls, c)
			if isWriteCall(c) {
				for _, lit := range goLiterals(c.Args) {
					written[lit] = true
				}
			}
		}
		return true
	})

	reads, namesSource := false, false
	for _, c := range calls {
		pkg, name := callName(c)
		switch {
		case pkg == "parser" && name == "ParseDir":
			return true
		case isReadCall(c):
			reads = true
		case pkg == "filepath" && name == "Glob" && len(c.Args) == 1 && strings.HasSuffix(stringLit(c.Args[0]), "*.go"):
			namesSource = true
		case pkg == "strings" && name == "HasSuffix" && len(c.Args) == 2 && stringLit(c.Args[1]) == ".go":
			namesSource = true
		}
	}
	for _, lit := range goLiterals([]ast.Expr{&ast.ParenExpr{X: fileLiterals(f)}}) {
		if !written[lit] {
			namesSource = true
		}
	}
	return reads && namesSource
}

// fileLiterals gathers every string literal in f into one composite so
// goLiterals can walk them with the same filter it applies to call arguments.
func fileLiterals(f *ast.File) ast.Expr {
	cl := &ast.CompositeLit{}
	ast.Inspect(f, func(n ast.Node) bool {
		if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
			cl.Elts = append(cl.Elts, bl)
		}
		return true
	})
	return cl
}

// isReadCall reports a call that reads a file from disk. parser.ParseFile
// reads only when its src argument is nil; with src it parses bytes in hand.
func isReadCall(c *ast.CallExpr) bool {
	pkg, name := callName(c)
	switch pkg + "." + name {
	case "os.ReadFile", "os.Open", "ioutil.ReadFile":
		return true
	case "parser.ParseFile":
		return len(c.Args) >= 3 && isNil(c.Args[2])
	}
	return false
}

func isWriteCall(c *ast.CallExpr) bool {
	pkg, name := callName(c)
	switch pkg + "." + name {
	case "os.WriteFile", "os.Create", "ioutil.WriteFile":
		return true
	}
	return strings.Contains(strings.ToLower(name), "write")
}

// callName returns ("pkg", "Func") for pkg.Func(...), ("", "func") for a bare
// call, and ("", "") for anything else.
func callName(c *ast.CallExpr) (string, string) {
	switch fn := c.Fun.(type) {
	case *ast.SelectorExpr:
		if id, ok := fn.X.(*ast.Ident); ok {
			return id.Name, fn.Sel.Name
		}
		return "", fn.Sel.Name
	case *ast.Ident:
		return "", fn.Name
	}
	return "", ""
}

// goLiterals collects every string literal ending in ".go" inside args,
// however deeply nested (filepath.Join(dir, "x.go") counts).
func goLiterals(args []ast.Expr) []string {
	var out []string
	for _, a := range args {
		ast.Inspect(a, func(n ast.Node) bool {
			if s := stringLit(n); goFileName.MatchString(s) {
				out = append(out, s)
			}
			return true
		})
	}
	return out
}

func stringLit(n ast.Node) string {
	bl, ok := n.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return ""
	}
	s, err := strconv.Unquote(bl.Value)
	if err != nil {
		return ""
	}
	return s
}

func isNil(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

func TestReadsGoSource(t *testing.T) {
	t.Parallel()
	body := func(imports, code string) []byte {
		return []byte("package p\n\nimport (\n" + imports + "\n)\n\nfunc f(dir string) {\n" + code + "\n}\n")
	}
	const osfp = `"os"; "path/filepath"`
	cases := []struct {
		name string
		src  []byte
		want bool
	}{
		{"literal path", body(`"os"`, `os.ReadFile("send.go")`), true},
		{"joined path", body(osfp, `os.ReadFile(filepath.Join(dir, "send.go"))`), true},
		{"path through a variable", body(osfp, `p := filepath.Join(dir, "send.go"); os.ReadFile(p)`), true},
		{"file names in a table", body(`"os"`, `for _, n := range []string{"relay.go"} { os.ReadFile(n) }`), true},
		{"open", body(osfp, `os.Open(filepath.Join(dir, "wshub.go"))`), true},
		{"glob of the package", body(osfp, `m, _ := filepath.Glob("*.go"); os.ReadFile(m[0])`), true},
		{"suffix scan", body(`"os"; "strings"`, `e, _ := os.ReadDir("."); if strings.HasSuffix(e[0].Name(), ".go") { os.ReadFile(e[0].Name()) }`), true},
		{"parse a directory", body(`"go/parser"; "go/token"`, `parser.ParseDir(token.NewFileSet(), ".", nil, 0)`), true},
		{"parse a file from disk", body(`"go/parser"; "go/token"`, `parser.ParseFile(token.NewFileSet(), "router.go", nil, 0)`), true},
		{"fixture written then read", body(osfp, `p := filepath.Join(dir, "main.go"); os.WriteFile(filepath.Join(dir, "main.go"), nil, 0o600); os.ReadFile(p)`), false},
		{"fixture written by a helper", body(`"os"`, `writeFile(dir, "main.go"); os.ReadFile("main.go")`), false},
		{"parse bytes in hand", body(`"go/parser"; "go/token"`, `parser.ParseFile(token.NewFileSet(), "x.go", []byte("package p"), 0)`), false},
		{"reads a non-Go file", body(`"os"`, `os.ReadFile("config.yaml")`), false},
		{"names a Go file without reading", body(`"fmt"`, `fmt.Println("send.go")`), false},
	}
	for _, tc := range cases {
		if got := readsGoSource(tc.src); got != tc.want {
			t.Errorf("%s: readsGoSource = %v, want %v\n%s", tc.name, got, tc.want, tc.src)
		}
	}
}

// TestSourceAnchorWalk_CheckoutNamedNaozhi: CI checks out into …/naozhi/naozhi
// and the owner's worktree is ~/workspace/naozhi. The walk has to count a tree
// whose own directory is called naozhi, skip only the old clone nested inside
// it, and skip vendored and fixture trees.
func TestSourceAnchorWalk_CheckoutNamedNaozhi(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "naozhi")
	reader := []byte("package p\n\nimport \"os\"\n\nfunc f() { os.ReadFile(\"send.go\") }\n")
	for _, p := range []string{
		"internal/server/send_test.go",
		"cmd/naozhi/main_test.go",
		"naozhi/internal/server/send_test.go",
		"test/e2e/node_modules/x/x_test.go",
		"internal/server/testdata/fixture_test.go",
	} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, reader, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A reason is a line of its own; the marker quoted in prose is not one.
	justified := append([]byte("// anchor-"+"keep: pins an import ban.\n"), reader...)
	quoted := append([]byte("// Files need a `// anchor-"+"keep:` line.\n"), reader...)
	for p, src := range map[string][]byte{"internal/a/justified_test.go": justified, "internal/a/quoted_test.go": quoted} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, src, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	anchors, unjustified, err := sourceAnchorFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	send := filepath.Join("internal", "server", "send_test.go")
	main := filepath.Join("cmd", "naozhi", "main_test.go")
	justifiedRel := filepath.Join("internal", "a", "justified_test.go")
	quotedRel := filepath.Join("internal", "a", "quoted_test.go")
	wantAnchors := []string{main, justifiedRel, quotedRel, send}
	wantUnjustified := []string{main, quotedRel, send}
	if !slices.Equal(anchors, wantAnchors) {
		t.Errorf("anchors = %v, want %v", anchors, wantAnchors)
	}
	if !slices.Equal(unjustified, wantUnjustified) {
		t.Errorf("unjustified = %v, want %v", unjustified, wantUnjustified)
	}
}

// A count below its baseline is a failure too: the PR that removes an anchor
// lowers the baseline, so the slack cannot be refilled later.
func TestAnchorCountProblem_BothDirections(t *testing.T) {
	t.Parallel()
	two := []string{"a_test.go", "b_test.go"}
	if msg := anchorCountProblem("files", two, 2, "base", ""); msg != "" {
		t.Errorf("equal counts reported %q", msg)
	}
	if msg := anchorCountProblem("files", two, 1, "base", ""); !strings.Contains(msg, "grew") {
		t.Errorf("growth reported %q", msg)
	}
	if msg := anchorCountProblem("files", two, 3, "base", ""); !strings.Contains(msg, "lower base to 2") {
		t.Errorf("a drop reported %q, want an instruction to lower the baseline", msg)
	}
}
