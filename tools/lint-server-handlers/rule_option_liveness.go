// rule option_liveness (#2897 S5): every field of HubOptions and
// sendEngineOpts must be read via a `<param>.Field` selector inside some
// function that takes a value of that type. struct_budget
// (rule_server_fields.go) pins the field COUNT; this is the companion check
// that a counted field actually earns its slot — AgentCmds sat in HubOptions
// with zero reads through three refactors because nothing ever asked whether
// each field was used, only how many there were.
//
// A field set only in a composite literal (the write side, e.g.
// build_dashboard.go's `HubOptions{Router: w.router, ...}`) does not count:
// that is where the option is populated, not where it is consumed.
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

// optionLivenessTypes are the opts structs this rule covers.
var optionLivenessTypes = []string{"HubOptions", "sendEngineOpts"}

// scanOptionLiveness implements rule option_liveness.
func scanOptionLiveness(pkgDir string) []Violation {
	fset := token.NewFileSet()
	var files []*ast.File
	var names []string
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return []Violation{{Rule: "option_liveness", File: filepath.ToSlash(pkgDir), Message: err.Error()}}
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(pkgDir, n), nil, parser.SkipObjectResolution)
		if err != nil {
			return []Violation{{Rule: "option_liveness", File: n, Message: err.Error()}}
		}
		files = append(files, f)
		names = append(names, n)
	}

	var out []Violation
	for _, typeName := range optionLivenessTypes {
		fields, declFile, _ := structFields(fset, files, names, typeName)
		if fields == nil {
			out = append(out, Violation{Rule: "option_liveness", File: filepath.ToSlash(pkgDir),
				Message: fmt.Sprintf("type %s struct not found — option_liveness has nothing to check; if it moved packages, move this rule's entry with it", typeName)})
			continue
		}
		read := map[string]bool{}
		for _, f := range files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				for _, v := range paramsOfType(fn, typeName) {
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
			out = append(out, Violation{Rule: "option_liveness", File: declFile, Line: fields[name],
				Message: fmt.Sprintf("%s.%s is never read via a <param>.%s selector in a function taking %s: delete the dead field, or add the read that was meant to use it", typeName, name, name, typeName)})
		}
	}
	return out
}

// paramsOfType names fn's parameters declared as plain (by-value) typeName —
// HubOptions and sendEngineOpts are both passed by value at their one call
// site each (NewHub, newSendEngine).
func paramsOfType(fn *ast.FuncDecl, typeName string) []string {
	var out []string
	if fn.Type.Params == nil {
		return out
	}
	for _, p := range fn.Type.Params.List {
		id, ok := p.Type.(*ast.Ident)
		if !ok || id.Name != typeName {
			continue
		}
		out = append(out, namesOf(p)...)
	}
	return out
}

// namesOf returns a parameter field's names.
func namesOf(p *ast.Field) []string {
	var out []string
	for _, n := range p.Names {
		out = append(out, n.Name)
	}
	return out
}
