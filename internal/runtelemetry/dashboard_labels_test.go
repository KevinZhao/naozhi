package runtelemetry

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// cronDashboardLabels is the dashboard's error-class label table, relative to
// this package.
const cronDashboardLabels = "../server/static/cron_format.js"

// notCronDashboardClasses are the frozen classes the cron dashboard never
// renders: the zero value, which means no error, and sysession's own classes.
var notCronDashboardClasses = map[ErrorClass]bool{
	ErrClassNone:                true,
	ErrClassSysessionUpstream:   true,
	ErrClassSysessionValidation: true,
}

// labelKey matches one `key: <string literal>` entry per line; the value may
// use any of JS's three quote characters.
var labelKey = regexp.MustCompile(`(?m)^\s*([A-Za-z0-9_]+):\s*['"` + "`]")

// TestCronErrorClassesHaveDashboardLabels: every frozen error class outside
// notCronDashboardClasses has a label in CRON_ERROR_CLASS_LABELS, and every
// label names a frozen class. wireErrorClasses is complete by
// TestEnumWireFreezeComplete, and cron's classes are re-exports of these
// constants, so a new cron class cannot reach the dashboard as its raw wire
// string, and a renamed one cannot leave its label behind.
func TestCronErrorClassesHaveDashboardLabels(t *testing.T) {
	t.Parallel()
	labels := cronDashboardLabelKeys(t)
	for c := range wireErrorClasses {
		switch {
		case notCronDashboardClasses[c] && labels[string(c)]:
			t.Errorf("error class %q has a dashboard label but is listed in notCronDashboardClasses", c)
		case !notCronDashboardClasses[c] && !labels[string(c)]:
			t.Errorf("error class %q has no label in CRON_ERROR_CLASS_LABELS (%s); the dashboard would show the raw string (only one `key: <string literal>` entry per line is recognised)", c, cronDashboardLabels)
		}
	}
	for k := range labels {
		if _, ok := wireErrorClasses[ErrorClass(k)]; !ok {
			t.Errorf("CRON_ERROR_CLASS_LABELS key %q matches no frozen error class", k)
		}
	}
}

// cronDashboardLabelKeys returns the keys of the CRON_ERROR_CLASS_LABELS
// object literal in cron_format.js.
func cronDashboardLabelKeys(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile(cronDashboardLabels)
	if err != nil {
		t.Fatal(err)
	}
	const open = "const CRON_ERROR_CLASS_LABELS = Object.freeze({"
	_, body, ok := strings.Cut(string(src), open)
	if !ok {
		t.Fatalf("%s: %q not found", cronDashboardLabels, open)
	}
	body, _, ok = strings.Cut(body, "\n});")
	if !ok {
		t.Fatalf("%s: CRON_ERROR_CLASS_LABELS has no closing \"});\"", cronDashboardLabels)
	}
	keys := map[string]bool{}
	for _, m := range labelKey.FindAllStringSubmatch(body, -1) {
		if keys[m[1]] {
			t.Errorf("CRON_ERROR_CLASS_LABELS key %q appears twice", m[1])
		}
		keys[m[1]] = true
	}
	return keys
}
