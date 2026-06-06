package cron

import (
	"testing"

	robfigcron "github.com/robfig/cron/v3"
)

// TestCronParseOptionsFieldSet pins the accepted-field bitmask so a refactor
// (or an accidental edit) that drops or adds a field is caught here rather
// than surfacing as a parse behaviour regression. R249-ARCH-24 (#988).
func TestCronParseOptionsFieldSet(t *testing.T) {
	want := robfigcron.Minute | robfigcron.Hour | robfigcron.Dom |
		robfigcron.Month | robfigcron.Dow | robfigcron.Descriptor
	if cronParseOptions != want {
		t.Fatalf("cronParseOptions = %b, want %b", cronParseOptions, want)
	}
}

// TestCronParserAcceptsExpectedForms verifies the parser built from
// cronParseOptions accepts the standard 5-field grammar and @descriptors,
// and rejects a 6-field (seconds) expression that the option set excludes.
func TestCronParserAcceptsExpectedForms(t *testing.T) {
	accept := []string{
		"*/5 * * * *", // every 5 minutes, 5-field
		"0 9 * * 1-5", // weekday 09:00
		"@daily",      // descriptor
		"@every 10m",  // descriptor interval
		"30 2 1 * *",  // monthly, day-of-month
	}
	for _, expr := range accept {
		if _, err := cronParser.Parse(expr); err != nil {
			t.Errorf("cronParser.Parse(%q) returned error, want accept: %v", expr, err)
		}
	}

	// Seconds field is intentionally NOT in cronParseOptions; a 6-field
	// expression must be rejected so the documented grammar stays stable.
	if _, err := cronParser.Parse("0 */5 * * * *"); err == nil {
		t.Errorf("cronParser.Parse 6-field (seconds) expression: want error, got nil")
	}
}
