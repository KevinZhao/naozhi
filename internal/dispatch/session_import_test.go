// anchor-keep: a per-file import ban (only resolver_fallback.go may import internal/session) is a structural fact go list cannot express; go/parser over the import blocks is the direct tool.

package dispatch

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestSessionImportedOnlyByTheResolverFallback: the dispatcher talks to the
// router, the session and the resolver through its consumer interfaces; the
// only production file that may import internal/session is
// resolver_fallback.go, which constructs the fallback KeyResolver.
func TestSessionImportedOnlyByTheResolverFallback(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var importers []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "github.com/naozhi/naozhi/internal/session" {
				importers = append(importers, name)
			}
		}
	}
	if len(importers) != 1 || importers[0] != "resolver_fallback.go" {
		t.Errorf("files importing internal/session = %v, want only resolver_fallback.go; use the interfaces in consumer.go and the sessionview / sessionkey leaves", importers)
	}
}
