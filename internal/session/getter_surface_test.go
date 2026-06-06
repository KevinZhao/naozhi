package session_test

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

// TestGetterSurfaceFrozen is the EFFECTIVE form of #463 Alternative C: an
// AST guard that freezes the exported Get*/Fetch* accessor surface of the
// internal/session and internal/cli production packages so a future PR
// cannot silently grow the getter naming sprawl that #463 wants to curb.
//
// Why a test and not a lint rule: .golangci.yml is NOT CI-wired (CI runs
// `go test` only — same situation TestPackageIsLeaf in internal/sessionkey
// documents), so a depguard/forbidigo rule would be unenforced. This test
// mirrors that AST-walker pattern (parser.ParseFile + ast.Inspect) and runs
// under the normal `go test` gate.
//
// Contract: any EXPORTED method whose name starts with Get or Fetch declared
// on a type in a scanned production file must be in the frozen allowlist. The
// allowlist is exactly the set that exists at #870/#463 freeze time (including
// the bare ScratchPool.Get map accessor), so the guard is GREEN on day one and
// only RED-fails when a NEW Get*/Fetch* getter is added
// — at which point the author must either rename it (the #463 direction) or
// consciously extend the allowlist in the same PR (recording the decision).
//
// Scope note: this scans internal/session + internal/cli only. node.FetchSessions
// (internal/node/httpclient.go, reverseconn.go) is deliberately OUT of scope and
// intentionally retains the `Fetch` verb — it performs a real network fetch over
// the reverse-RPC / HTTP transport, so `Fetch` is the correct, descriptive verb
// (it is NOT a local accessor). This corrects the RFC's over-broad "Fetch->Load"
// rename rule: Load implies in-memory/disk retrieval, which would mislabel a
// network round-trip. The #463 guard therefore freezes local accessors here and
// leaves the network-fetch verb alone.
//
// _test.go files are excluded (test fakes may name methods however they like).
// testutil.go is NOT excluded — it is `//go:build !release` but lacks the
// _test.go suffix, so it is part of the default production build and its
// exported TestProcess methods State/SessionID (renamed from GetState/GetSessionID
// by ADR-0001 PR-2) carry no Get prefix, so they are not surface here.
func TestGetterSurfaceFrozen(t *testing.T) {
	t.Parallel()

	// frozen allowlist == the exact exported Get*/Fetch* accessor set present
	// in internal/session + internal/cli at freeze time. Kept as the spec's
	// named set; GetActiveCount / GetCurrentBackend are reserved names from the
	// #870 lifecycle contract (not yet declared) and are harmless extras — the
	// guard only flags symbols OUTSIDE the allowlist, never missing ones.
	allow := map[string]bool{
		"Get":               true, // scratch.go: *ScratchPool (map-style accessor)
		"GetOrCreate":       true, // router_lifecycle.go: *Router (get-or-create compound semantics, intentionally retained per ADR-0001 PR-2)
		"GetActiveCount":    true, // reserved (#870 lifecycle contract)
		"GetCurrentBackend": true, // reserved (#870 lifecycle contract)
	}

	// repoRoot: this test runs with CWD = internal/session. Walk up to the
	// module root so we can reach the sibling internal/cli package without
	// hard-coding an absolute path.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoRoot := filepath.Clean(filepath.Join(wd, "..", ".."))

	dirs := []string{
		filepath.Join(repoRoot, "internal", "session"),
		filepath.Join(repoRoot, "internal", "cli"),
	}

	type offender struct{ file, name string }
	var offenders []offender
	fset := token.NewFileSet()

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir %q: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok || fn.Recv == nil { // methods only (accessors have a receiver)
					return true
				}
				mn := fn.Name.Name
				if !fn.Name.IsExported() {
					return true
				}
				if !strings.HasPrefix(mn, "Get") && !strings.HasPrefix(mn, "Fetch") {
					return true
				}
				if allow[mn] {
					return true
				}
				offenders = append(offenders, offender{file: name, name: mn})
				return true
			})
		}
	}

	if len(offenders) > 0 {
		sort.Slice(offenders, func(i, j int) bool {
			if offenders[i].file != offenders[j].file {
				return offenders[i].file < offenders[j].file
			}
			return offenders[i].name < offenders[j].name
		})
		var b strings.Builder
		for _, o := range offenders {
			b.WriteString("\n  ")
			b.WriteString(o.file)
			b.WriteString(": ")
			b.WriteString(o.name)
		}
		t.Fatalf("new exported Get*/Fetch* accessor(s) outside the frozen #463 allowlist:%s\n\n"+
			"This is the effective #463 Alternative C guard. Either rename the accessor "+
			"(drop the Get/Fetch prefix per Go convention — the #463 direction), or, if the "+
			"name is intentional, add it to the allowlist in this test in the SAME PR so the "+
			"naming decision is recorded.", b.String())
	}
}
