package runstore

import (
	"strings"
	"testing"
)

// TestValidID_AcceptsOnlyLowercaseHex: IDs become runs/<jobID>/<runID>.json
// path components, so anything beyond 1–64 lowercase hex chars is refused.
func TestValidID_AcceptsOnlyLowercaseHex(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]bool{
		"0123456789abcdef":      true,
		strings.Repeat("a", 64): true,
		"":                      false,
		strings.Repeat("a", 65): false,
		"0123456789ABCDEF":      false,
		"../etc":                false,
		"abc.json":              false,
		"abc.tmp":               false,
	} {
		if got := ValidID(id); got != want {
			t.Errorf("ValidID(%q) = %v, want %v", id, got, want)
		}
	}
}
