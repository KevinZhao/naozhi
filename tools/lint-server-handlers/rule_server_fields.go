// Rules struct_budget and server_field_liveness (#2897 S4): Server keeps only
// what something reads after construction.
//
//	struct_budget: Server has exactly serverFieldBaseline fields. More is a new
//	  field to justify; fewer means the baseline is lowered in the same change,
//	  so the room cannot be refilled. #2897 S5 reuses the same check for two
//	  more structs via additionalStructBudgets: HubOptions and sendEngineOpts
//	  are plain config/opts carriers, so they get the count pin without
//	  server_field_liveness's build-step distinction — their dead-field
//	  problem is option_liveness's job (rule_option_liveness.go) instead.
//	server_field_liveness: every Server field is read by some function that
//	  is not a build step (build*, register*, New*, new*). A field only
//	  construction reads belongs in the construction's own locals (wiring).
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// serverFieldBaseline is Server's field count.
const serverFieldBaseline = 25

// hubOptionsFieldBaseline is HubOptions's field count (#2897 S5a dropped the
// dead AgentCmds; S5c2 dropped the five fields only sendEngineOpts reads and
// added Engine/Broadcaster).
const hubOptionsFieldBaseline = 16

// sendEngineOptsFieldBaseline is sendEngineOpts's field count. The turn
// pipeline arrives as the one Turns field; cron's prompt saver is
// turnSender's, not the engine's.
const sendEngineOptsFieldBaseline = 9

// additionalStructBudgets are the struct_budget subjects besides Server:
// plain opts carriers pinned on count alone (server_field_liveness's
// build-step distinction does not apply to them; option_liveness covers
// their dead-field case instead).
var additionalStructBudgets = []struct {
	TypeName     string
	BaselineName string
	Baseline     int
}{
	{"HubOptions", "hubOptionsFieldBaseline", hubOptionsFieldBaseline},
	{"sendEngineOpts", "sendEngineOptsFieldBaseline", sendEngineOptsFieldBaseline},
}

// isBuildStep reports whether a function name is a construction step.
func isBuildStep(name string) bool {
	for _, p := range []string{"build", "register", "New", "new"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func scanServerFields(pkgDir string) []Violation {
	fset := token.NewFileSet()
	var files []*ast.File
	var names []string
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return []Violation{{Rule: "struct_budget", File: pkgDir, Message: err.Error()}}
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(pkgDir, n), nil, parser.SkipObjectResolution)
		if err != nil {
			return []Violation{{Rule: "struct_budget", File: n, Message: err.Error()}}
		}
		files = append(files, f)
		names = append(names, n)
	}

	fields, declFile, declLine := structFields(fset, files, names, "Server")
	if fields == nil {
		return []Violation{{Rule: "struct_budget", File: pkgDir, Message: "type Server struct not found"}}
	}
	var out []Violation
	switch n := len(fields); {
	case n > serverFieldBaseline:
		out = append(out, Violation{Rule: "struct_budget", File: declFile, Line: declLine,
			Message: fmt.Sprintf("Server has %d fields, above the baseline of %d: keep construction-only dependencies in wiring, or justify the field", n, serverFieldBaseline)})
	case n < serverFieldBaseline:
		out = append(out, Violation{Rule: "struct_budget", File: declFile, Line: declLine,
			Message: fmt.Sprintf("Server has %d fields: lower serverFieldBaseline to %d", n, n)})
	}

	for _, b := range additionalStructBudgets {
		bFields, bFile, bLine := structFields(fset, files, names, b.TypeName)
		if bFields == nil {
			out = append(out, Violation{Rule: "struct_budget", File: pkgDir,
				Message: fmt.Sprintf("type %s struct not found — %s has nothing to check; if it moved, move this entry with it", b.TypeName, b.BaselineName)})
			continue
		}
		switch n := len(bFields); {
		case n > b.Baseline:
			out = append(out, Violation{Rule: "struct_budget", File: bFile, Line: bLine,
				Message: fmt.Sprintf("%s has %d fields, above the baseline of %d: justify the new field", b.TypeName, n, b.Baseline)})
		case n < b.Baseline:
			out = append(out, Violation{Rule: "struct_budget", File: bFile, Line: bLine,
				Message: fmt.Sprintf("%s has %d fields: lower %s to %d", b.TypeName, n, b.BaselineName, n)})
		}
	}

	read := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || isBuildStep(fn.Name.Name) {
				continue
			}
			for _, v := range serverVars(fn) {
				markReads(fn.Body, v, read)
			}
		}
	}
	var dead []string
	for name := range fields {
		if !read[name] {
			dead = append(dead, name)
		}
	}
	sort.Strings(dead)
	for _, name := range dead {
		out = append(out, Violation{Rule: "server_field_liveness", File: declFile, Line: fields[name],
			Message: fmt.Sprintf("Server.%s is read only by build steps: make it a construction local (wiring)", name)})
	}
	return out
}

// structFields returns typeName's field names with their lines, and where
// the struct is declared, scanning files already parsed from one package dir.
// Shared by struct_budget (for Server and additionalStructBudgets) and
// option_liveness (rule_option_liveness.go).
func structFields(fset *token.FileSet, files []*ast.File, names []string, typeName string) (map[string]int, string, int) {
	for i, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, sp := range gd.Specs {
				ts, ok := sp.(*ast.TypeSpec)
				if !ok || ts.Name.Name != typeName {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				out := map[string]int{}
				for _, fl := range st.Fields.List {
					for _, n := range fl.Names {
						out[n.Name] = fset.Position(n.Pos()).Line
					}
				}
				return out, names[i], fset.Position(ts.Pos()).Line
			}
		}
	}
	return nil, "", 0
}

// serverVars names the *Server values fn can read fields from: its receiver
// and its *Server parameters.
func serverVars(fn *ast.FuncDecl) []string {
	var out []string
	add := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, p := range fl.List {
			star, ok := p.Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if id, ok := star.X.(*ast.Ident); !ok || id.Name != "Server" {
				continue
			}
			for _, n := range p.Names {
				out = append(out, n.Name)
			}
		}
	}
	add(fn.Recv)
	add(fn.Type.Params)
	return out
}

// markReads records v.<field> selectors in body that are not the target of
// an assignment.
func markReads(body *ast.BlockStmt, v string, read map[string]bool) {
	written := map[*ast.SelectorExpr]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok {
			for _, l := range as.Lhs {
				if sel, ok := l.(*ast.SelectorExpr); ok {
					written[sel] = true
				}
			}
		}
		return true
	})
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || written[sel] {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == v {
			read[sel.Sel.Name] = true
		}
		return true
	})
}
