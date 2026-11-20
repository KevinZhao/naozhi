package agent

import "testing"

// A lookup's answer is outside content: its turn is judged like a scout's.
func TestLookupSourceOutside(t *testing.T) {
	if !SourceLookup.Outside() || outsideLabels[SourceLookup] == "" {
		t.Fatal("a lookup's answer turn is not an outside turn")
	}
}
