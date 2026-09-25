// static_toplevel_views_contract_test.go — what is left of the activity-rail
// source contracts. Rail navigation, view switching, the rail badges and the
// 系统 poll lifecycle are driven in test/e2e/activity_rail_views.test.js.
package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_ValidDotClassesIncludesUnreachable pins R20260606-CODE-3:
// 'unreachable' must be in VALID_DOT_CLASSES so the settings-view conn dot and
// ns-dot elements receive the correct CSS class (whose rules exist in
// dashboard.html) instead of falling back to 'offline'.
func TestDashboardJS_ValidDotClassesIncludesUnreachable(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)

	if !strings.Contains(js, `unreachable: 'unreachable'`) {
		t.Error("VALID_DOT_CLASSES must include unreachable: 'unreachable' so the CSS rule is not a dead rule")
	}
}
