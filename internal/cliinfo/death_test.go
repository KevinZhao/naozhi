package cliinfo

import (
	"os"
	"regexp"
	"slices"
	"testing"
)

// anchor-keep: reads death.go to keep AllDeathReasons in step with its
// DeathReason* constants; that pairing is the whole point of the test.

// deathReasonConstRe matches one DeathReason* constant's name and value.
var deathReasonConstRe = regexp.MustCompile(`(DeathReason\w+)\s+= "([a-z_]+)"`)

// TestAllDeathReasons_MatchesDeathGo reads death.go and asserts
// AllDeathReasons is exactly the set of DeathReason* constant values declared
// there — in either direction: a constant added without updating the slice,
// or a slice entry whose constant was removed.
func TestAllDeathReasons_MatchesDeathGo(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("death.go")
	if err != nil {
		t.Fatal(err)
	}
	matches := deathReasonConstRe.FindAllStringSubmatch(string(src), -1)
	if len(matches) < 5 {
		t.Fatalf("found only %d DeathReason* constants in death.go, below any real count: the regex has gone blind", len(matches))
	}
	var want []string
	for _, m := range matches {
		want = append(want, m[2])
	}
	got := slices.Clone(AllDeathReasons())
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("AllDeathReasons() = %v, death.go declares %v", AllDeathReasons(), want)
	}
	// slices.Equal on the sorted copies would also pass if AllDeathReasons
	// dropped one value and repeated another; the raw length catches that.
	if len(AllDeathReasons()) != len(matches) {
		t.Fatalf("AllDeathReasons() has %d entries, death.go declares %d constants — a duplicate value?", len(AllDeathReasons()), len(matches))
	}
}

func TestAllSessionStates(t *testing.T) {
	t.Parallel()
	if got, want := AllSessionStates(), []string{"dead", "ready", "running"}; !slices.Equal(got, want) {
		t.Errorf("AllSessionStates() = %v, want %v", got, want)
	}
}
