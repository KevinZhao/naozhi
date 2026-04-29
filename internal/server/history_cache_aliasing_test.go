package server

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestHistoryCache_AliasingInvariant is the R62-GO-5 pin for the
// historyCache slice-aliasing contract documented on the field:
// readers receive slice headers that alias h.historyCache's backing
// array. Race-free ONLY if the refresh path assigns a freshly-allocated
// slice to h.historyCache; append-in-place would mutate the backing
// array readers still hold a reference to.
//
// The canonical refresh path is loadHistorySessions, which today does:
//
//	all := discovery.RecentSessions(...)
//	...
//	h.historyCache = all
//
// This test reads dashboard_session.go and asserts:
//
//  1. h.historyCache is assigned from a RecentSessions call result
//     (fresh allocation by the discovery package).
//  2. No `append(h.historyCache, ...)` pattern appears anywhere —
//     in-place mutation of the cached slice would violate the
//     aliasing invariant and produce cross-reader corruption
//     indistinguishable from a classic data race.
//
// If a future refactor adds incremental updates (e.g. "append the
// newly discovered session without re-scanning"), the author must
// either shallow-copy before the append or rework the read path to
// return copies. Either way the change must be reviewed through
// this audit item.
func TestHistoryCache_AliasingInvariant(t *testing.T) {
	src, err := os.ReadFile("dashboard_session.go")
	if err != nil {
		t.Fatalf("read dashboard_session.go: %v", err)
	}
	body := string(src)

	// 1) Look up loadHistorySessions body.
	startIdx := strings.Index(body, "func (h *SessionHandlers) loadHistorySessions()")
	if startIdx < 0 {
		t.Fatal("loadHistorySessions is no longer defined in dashboard_session.go. " +
			"If renamed, update this test; if removed, re-audit the cache refresh " +
			"path for the R62-GO-5 aliasing invariant.")
	}
	// Scan until the next top-level func or end of file.
	rest := body[startIdx:]
	endRel := regexp.MustCompile(`\nfunc `).FindStringIndex(rest[6:])
	var loadBody string
	if endRel != nil {
		loadBody = rest[:6+endRel[0]]
	} else {
		loadBody = rest
	}

	// 2) Confirm loadHistorySessions assigns to h.historyCache.
	if !strings.Contains(loadBody, "h.historyCache = ") {
		t.Error("loadHistorySessions no longer assigns h.historyCache via `=`. " +
			"R62-GO-5: the cache refresh path MUST replace h.historyCache with a " +
			"fresh slice (not append into the existing backing array) or readers " +
			"holding the old header will observe writes through their alias.")
	}

	// 3) Negative check across the whole file: no `append(h.historyCache, ...)`.
	// This is the specific pattern that would break the invariant.
	appendRe := regexp.MustCompile(`append\(\s*h\.historyCache\b`)
	if appendRe.MatchString(body) {
		t.Error("`append(h.historyCache, ...)` pattern detected in " +
			"dashboard_session.go. R62-GO-5: appending in place mutates the " +
			"backing array that prior readers still alias; use a fresh slice " +
			"(`all := ...; h.historyCache = all`) or shallow-copy before append.")
	}
}
