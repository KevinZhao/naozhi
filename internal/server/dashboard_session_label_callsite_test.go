package server

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestSetLabel_AuditSlogCallsRunSanitizeLogAttr_SourcePin pins R246-SEC-14
// (#820) at the call-site rather than the sanitiser primitive.
//
// dashboard_session_label_sanitize_test.go already pins that
// session.SanitizeLogAttr scrubs log-injection runes — but it cannot
// detect a regression that *removes* the call from handleSetLabel and
// passes req.Node / req.Key raw to slog. Both branches (remote-failure
// path AND remote-success / local-success audit paths) share the same
// "session label updated" Info or "remote set session label failed"
// Warn key, and both must wrap the node + key fields.
//
// This test reads the source of dashboard_session.go and asserts that:
//
//  1. Every slog call in handleSetLabel that mentions a "node" or "key"
//     attr passes a SanitizeLogAttr-wrapped expression for that attr,
//     OR a constant string ("local") for the node attr.
//  2. The number of audit slog call-sites covered is at least 3 (warn
//     path + remote info path + local info path), so a future
//     restructure that drops a branch fails this test instead of
//     silently shrinking the audit surface.
//
// We rely on grep over the source rather than reflection because slog
// arguments are interface{} — at runtime we cannot tell whether
// SanitizeLogAttr was applied or req.Node was passed raw.
func TestSetLabel_AuditSlogCallsRunSanitizeLogAttr_SourcePin(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("dashboard_session.go")
	if err != nil {
		t.Fatalf("read dashboard_session.go: %v", err)
	}
	text := string(src)

	// Locate the handleSetLabel function body. The grep boundary is the
	// next top-level `func ` declaration. Anything outside this slice is
	// not the audit surface we're pinning.
	startIdx := strings.Index(text, "func (h *SessionHandlers) handleSetLabel(")
	if startIdx < 0 {
		t.Fatal("handleSetLabel not found in dashboard_session.go — was it renamed? update this test alongside.")
	}
	// Find the end of the function: the next "\nfunc " after startIdx.
	rest := text[startIdx+1:]
	endRel := strings.Index(rest, "\nfunc ")
	if endRel < 0 {
		endRel = len(rest)
	}
	body := text[startIdx : startIdx+1+endRel]

	// Extract every slog.{Info,Warn,Error,Debug}(... ) call in body.
	// Naive regex is enough: slog calls are stylised across this codebase
	// and the multiline arg list is bounded by the closing `)` at the end
	// of the call. We use a non-greedy capture and then a heuristic on
	// whether "node" / "key" attrs appear with a literal SanitizeLogAttr
	// or "local" partner.
	callRe := regexp.MustCompile(`(?s)slog\.(?:Info|Warn|Error|Debug)\(([^)]*?(?:\([^)]*\)[^)]*?)*)\)`)
	calls := callRe.FindAllString(body, -1)
	if len(calls) == 0 {
		t.Fatal("no slog calls found in handleSetLabel body — regex probably stale; update the test.")
	}

	covered := 0
	for _, call := range calls {
		hasNode := strings.Contains(call, `"node"`)
		hasKey := strings.Contains(call, `"key"`)
		if !hasNode && !hasKey {
			continue
		}
		// node may be either SanitizeLogAttr(req.Node) (remote paths)
		// or a constant "local" (local path).
		if hasNode {
			nodeOK := strings.Contains(call, `SanitizeLogAttr(req.Node)`) ||
				strings.Contains(call, `"node", "local"`)
			if !nodeOK {
				t.Errorf("slog call mentions \"node\" attr without SanitizeLogAttr or \"local\" guard:\n  %s\nfix: wrap req.Node through session.SanitizeLogAttr per dispatch/commands.go pattern.", call)
			}
		}
		if hasKey {
			if !strings.Contains(call, `SanitizeLogAttr(req.Key)`) {
				t.Errorf("slog call mentions \"key\" attr without SanitizeLogAttr wrapping:\n  %s\nfix: wrap req.Key through session.SanitizeLogAttr per dispatch/commands.go pattern.", call)
			}
		}
		covered++
	}

	// At time of fix the audit surface comprises:
	//
	//   - "remote set session label failed" (warn, both fields)
	//   - "session label updated" remote success (info, both fields)
	//   - "session label updated" local success (info, key only)
	//
	// Anything below 3 means a branch was dropped without updating the
	// pin or the surrounding contract.
	if covered < 3 {
		t.Errorf("only %d audit slog call-sites covered in handleSetLabel; expected ≥3 (warn / remote-info / local-info). a branch was likely removed — review the audit surface before relaxing this assertion.", covered)
	}
}
