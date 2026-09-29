// Rule sublock_encapsulation (#2897 S6): a sub-object's lock is taken by the
// sub-object's own methods. A call shaped X.f.mu.Lock() reaches through a
// field into another object's mutex, so that object can no longer change
// what the lock guards without auditing its owner's callers.
//
// Production code (non-test files under the scanned dirs) has none. Test
// files hold sublockTestBaseline such calls: more is a violation, fewer means
// the baseline is lowered in the same change.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
)

// sublockTestBaseline is the number of reach-through lock calls in test files.
const sublockTestBaseline = 38

var lockMethods = map[string]bool{
	"Lock": true, "Unlock": true, "RLock": true, "RUnlock": true, "TryLock": true, "TryRLock": true,
}

// reachThroughLock reports whether call is X.f.m.<lock method>().
func reachThroughLock(call *ast.CallExpr) bool {
	method, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !lockMethods[method.Sel.Name] {
		return false
	}
	lock, ok := method.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	_, ok = lock.X.(*ast.SelectorExpr)
	return ok
}

func scanSublocks(dirs []string, testBaseline int) []Violation {
	var out []Violation
	testHits := 0
	fset := token.NewFileSet()
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				out = append(out, Violation{Rule: "sublock_encapsulation", File: filepath.ToSlash(path), Message: err.Error()})
				return nil
			}
			isTest := strings.HasSuffix(path, "_test.go")
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !reachThroughLock(call) {
					return true
				}
				if isTest {
					testHits++
					return true
				}
				out = append(out, Violation{Rule: "sublock_encapsulation", File: filepath.ToSlash(path), Line: fset.Position(call.Pos()).Line,
					Message: "takes another object's lock through a field: give that object a method that locks for itself"})
				return true
			})
			return nil
		})
	}
	switch {
	case testHits > testBaseline:
		out = append(out, Violation{Rule: "sublock_encapsulation", File: "tools/lint-server-handlers/rule_sublock.go",
			Message: fmt.Sprintf("test files take a sub-object's lock through a field %d times, above the baseline of %d: go through the sub-object's methods", testHits, testBaseline)})
	case testHits < testBaseline:
		out = append(out, Violation{Rule: "sublock_encapsulation", File: "tools/lint-server-handlers/rule_sublock.go",
			Message: fmt.Sprintf("test files take a sub-object's lock through a field %d times: lower sublockTestBaseline to %d", testHits, testHits)})
	}
	return out
}
