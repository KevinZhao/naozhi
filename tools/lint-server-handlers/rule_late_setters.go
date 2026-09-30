// Rule no_late_setters (#2897 S7): the core runtime types take their
// collaborators at construction. A setter that injects a callback or a
// dependency after New leaves the object half-wired until someone calls it,
// and lets the wiring be swapped out from under running goroutines — which is
// why every one of them had to sit behind an atomic.Pointer. S7 moved each of
// them into the constructor; where the dependency is built later (a cycle),
// the host passes a relay and binds it once (internal/routerrelay,
// runtelemetry.Relay).
//
// AST only, so the shape is a heuristic: an exported method named Set… whose
// single parameter is a func type, or whose name says it installs behaviour
// (SetOn…, …Func, …Hook, …Callback, …Handler, …Observer, …Telemetry,
// …Ownership). Plain data setters (SetBackend(id string)) are not matched.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// lateSetterPkgs are the core runtime packages, relative to internal/.
var lateSetterPkgs = []string{"session", "cron", "sysession", "upstream"}

var lateSetterName = regexp.MustCompile(`^Set(On[A-Z]\w*|\w*(Func|Hook|Callback|Handler|Observer|Telemetry|Ownership))$`)

// isLateSetter reports whether fd has the late-injection shape.
func isLateSetter(fd *ast.FuncDecl) bool {
	if fd.Recv == nil || !strings.HasPrefix(fd.Name.Name, "Set") { // capital S: exported
		return false
	}
	if lateSetterName.MatchString(fd.Name.Name) {
		return true
	}
	params := fd.Type.Params.List
	if len(params) != 1 || len(params[0].Names) > 1 {
		return false
	}
	_, isFunc := params[0].Type.(*ast.FuncType)
	return isFunc
}

func scanLateSetters(dirs []string) []Violation {
	var out []Violation
	fset := token.NewFileSet()
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				out = append(out, Violation{Rule: "no_late_setters", File: filepath.ToSlash(path), Message: err.Error()})
				return nil
			}
			for _, decl := range f.Decls {
				if fd, ok := decl.(*ast.FuncDecl); ok && isLateSetter(fd) {
					out = append(out, Violation{Rule: "no_late_setters", File: filepath.ToSlash(path), Line: fset.Position(fd.Pos()).Line,
						Message: fd.Name.Name + " injects behaviour after construction: take it as a constructor argument (a relay bound once where the dependency is built later)"})
				}
			}
			return nil
		})
	}
	return out
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
