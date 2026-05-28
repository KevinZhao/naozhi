package session

import (
	"os"
	"strings"
	"testing"
)

// TestLookupSummariesCached_FiltersAlreadySummarized pins R040034-PERF-4
// (#1403) at the source level: the lookup-input-build closure inside
// lookupSummariesCached must skip snapshots whose Summary is already
// populated. Without the filter, the leader's per-cache-miss walk
// invokes discovery.LookupSummaries with the full snapshot set, paying
// projDir-grouping + sessions-index reads for sessions that already
// have a title. The fill loop in fillProjectAndSummary
// (`if summary := summaryMap[id]; summary != ""`) is a no-op on missing
// keys, so dropping already-summarised snapshots from the lookup input
// keeps the wire output identical — those snapshots keep their existing
// Summary value untouched on the next poll.
//
// This is a source-level pin: a runtime test would need a fake claudeDir
// with sessions-index.json fixtures to drive LookupSummaries through its
// real path, and the filter is a one-line invariant that's much more
// reliably guarded by reading the function body. A future "simplify"
// pass that drops the filter would silently re-introduce the redundant
// disk fan-out.
func TestLookupSummariesCached_FiltersAlreadySummarized(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatalf("read handlers.go: %v", err)
	}
	body := string(src)

	startMarker := "func (h *Handlers) lookupSummariesCached("
	startIdx := strings.Index(body, startMarker)
	if startIdx < 0 {
		t.Fatalf("could not locate lookupSummariesCached function — has the symbol moved or been renamed?")
	}
	// Find the end of the function by scanning forward to the next
	// top-level `\nfunc ` after the body opens. cheap enough for a
	// regression source pin.
	rest := body[startIdx:]
	endIdx := strings.Index(rest[1:], "\nfunc ")
	if endIdx < 0 {
		t.Fatalf("could not find end of lookupSummariesCached")
	}
	fnBody := rest[:endIdx+1]

	// Filter must skip already-summarised snapshots. The exact predicate
	// is `if snap.Summary != "" { continue }`; we anchor on the snippet
	// rather than parse the AST so a future formatter that rewrites the
	// branch to an early-skip pattern still matches as long as the
	// Summary != "" guard exists.
	if !strings.Contains(fnBody, "snap.Summary != \"\"") {
		t.Fatalf("lookupSummariesCached must filter out snapshots whose Summary is already populated " +
			"(`if snap.Summary != \"\" { continue }`) so the leader's lookup input shrinks to only " +
			"new sessions. R040034-PERF-4 (#1403) regression: a future refactor that drops the guard " +
			"silently re-introduces the per-cache-miss walk over already-titled snapshots.")
	}

	// Defensive: the `continue` below the Summary guard must remain so
	// the filter actually skips; an `if snap.Summary != "" {}` empty
	// branch would compile but degrade silently.
	guard := strings.Index(fnBody, "snap.Summary != \"\"")
	if guard < 0 {
		t.Fatal("guard locator regressed; see prior assertion")
	}
	tail := fnBody[guard:]
	if !strings.Contains(tail[:200], "continue") {
		t.Errorf("Summary != \"\" guard must be paired with a `continue` to actually skip the snapshot; " +
			"otherwise the filter is a no-op and the redundant lookup path is restored.")
	}
}
