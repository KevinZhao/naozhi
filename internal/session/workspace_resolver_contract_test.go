package session

import (
	"os"
	"regexp"
	"testing"
)

// TestWorkspaceResolution_SingleSiteContract pins R222-ARCH-12 (#735): the
// workspace decision (opts.Workspace > workspaceOverrides[chatKey] > old
// session workspace > router default) MUST live in exactly one place —
// resolveSpawnParamsLocked. Earlier rounds had this logic copy-pasted
// across spawnSession / Resume / ResetAndRecreate; centralisation
// happened in R70-ARCH-H2 (extracted into spawnParams) but no contract
// test pinned the invariant, so a future "quick fix" could silently
// reintroduce the duplication.
//
// We grep the lifecycle file for the canonical write pattern
// `workspace = opts.Workspace`. There must be exactly ONE such site
// (inside resolveSpawnParamsLocked). The pre-fix shape of #735
// (separate resolution branches in Resume/ResetAndRecreate) would
// re-add this assignment in two more spots and trip the assertion.
//
// Excluded: comments, test fixtures, and assignments scoped to other
// fields (e.g. `spawnOpts.Workspace = …`). The regex matches the bare
// local-variable assignment only.
func TestWorkspaceResolution_SingleSiteContract(t *testing.T) {
	body, err := os.ReadFile("router_lifecycle.go")
	if err != nil {
		t.Fatalf("read router_lifecycle.go: %v", err)
	}
	// The canonical assignment is `workspace = opts.Workspace` — the
	// merge step that takes the per-request override. Any duplicate
	// would copy this exact line.
	re := regexp.MustCompile(`(?m)^\s*workspace\s*=\s*opts\.Workspace\b`)
	matches := re.FindAllIndex(body, -1)
	if len(matches) != 1 {
		t.Fatalf("R222-ARCH-12 (#735) contract broken: workspace decision must live "+
			"in exactly one place (resolveSpawnParamsLocked). Found %d "+
			"`workspace = opts.Workspace` sites; expected 1. If you intentionally "+
			"reintroduced a second workspace resolver, route it through "+
			"resolveSpawnParamsLocked or update this test with the new contract.",
			len(matches))
	}
	// Sanity: the surviving site must sit within resolveSpawnParamsLocked,
	// not bare-floating in spawnSession or ResetAndRecreate. Find the
	// preceding `func` declaration.
	idx := matches[0][0]
	prefix := body[:idx]
	funcRe := regexp.MustCompile(`(?m)^func \(r \*Router\) (\w+)`)
	allFuncs := funcRe.FindAllSubmatch(prefix, -1)
	if len(allFuncs) == 0 {
		t.Fatal("could not locate enclosing func for workspace assignment")
	}
	enclosing := string(allFuncs[len(allFuncs)-1][1])
	if enclosing != "resolveSpawnParamsLocked" {
		t.Errorf("workspace decision moved out of resolveSpawnParamsLocked into %q. "+
			"Move it back, or update this contract test if the refactor is intentional.",
			enclosing)
	}
}
