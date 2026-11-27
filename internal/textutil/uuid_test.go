package textutil

import (
	"testing"
)

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
