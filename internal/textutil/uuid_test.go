package textutil

import (
	"regexp"
	"testing"
)

// TestDeriveLegacyUUID_Deterministic is the foundation of MergedSource's
// dedup: the same (time, type, summary, detail) tuple MUST always derive
// the same UUID. Breakage here would cause old Claude JSONL replays to
// re-duplicate existing naozhi-local entries.
func TestDeriveLegacyUUID_Deterministic(t *testing.T) {
	t.Parallel()
	got1 := DeriveLegacyUUID(1700000000000, "user", "hi", "")
	got2 := DeriveLegacyUUID(1700000000000, "user", "hi", "")
	if got1 != got2 {
		t.Errorf("non-deterministic: %q vs %q", got1, got2)
	}
}

// TestDeriveLegacyUUID_InputsAffectOutput: changing any input must change
// the output. Guards against accidental hash-input reduction.
func TestDeriveLegacyUUID_InputsAffectOutput(t *testing.T) {
	t.Parallel()
	base := DeriveLegacyUUID(1, "user", "hi", "")
	cases := []struct {
		name string
		got  string
	}{
		{"time", DeriveLegacyUUID(2, "user", "hi", "")},
		{"type", DeriveLegacyUUID(1, "text", "hi", "")},
		{"summary", DeriveLegacyUUID(1, "user", "hello", "")},
		{"detail", DeriveLegacyUUID(1, "user", "hi", "more")},
	}
	for _, tc := range cases {
		if tc.got == base {
			t.Errorf("%s change did not alter UUID: still %q", tc.name, base)
		}
	}
}

// TestDeriveLegacyUUID_Shape locks the width so it matches newEventUUID's
// shape — MergedSource dedups UUID strings directly and must not have to
// special-case one shape vs the other.
func TestDeriveLegacyUUID_Shape(t *testing.T) {
	t.Parallel()
	re := regexp.MustCompile(`^[0-9a-f]{32}$`)
	if got := DeriveLegacyUUID(1, "user", "hi", ""); !re.MatchString(got) {
		t.Errorf("DeriveLegacyUUID shape mismatch: %q", got)
	}
}

// TestDeriveLegacyUUID_StableAcrossPackageMove pins the v1 hash output for
// a fixed set of inputs. After the textutil migration two tiers of dedup
// (in-memory MergedSource + on-disk persisted entries) trust this exact
// 16-byte hex value to recognise existing legacy entries; if the hash
// changes silently the dedup window resets and the dashboard double-shows
// every replayed Claude JSONL entry.
//
// To regenerate intentionally: bump the "v1\x00" prefix in uuid.go to
// "v2\x00" AND update this golden, AND add a CHANGELOG note about the
// dedup-window reset cost.
func TestDeriveLegacyUUID_StableAcrossPackageMove(t *testing.T) {
	t.Parallel()
	const want = "e8d0ee9e885639c6454c2e415156a05c"
	got := DeriveLegacyUUID(1700000000000, "user", "hi", "")
	if got != want {
		t.Errorf("hash drifted: got %q want %q", got, want)
	}
}
