package discovery

import (
	"encoding/json"
	"testing"
)

// TestMaxSafeJSONInt_MatchesJavaScript pins the constant to the value JS
// hard-codes as Number.MAX_SAFE_INTEGER. This is pure numeric contract
// verification (no syscalls / platform) — a regression here means someone
// changed the constant thinking it was an arbitrary cap, whereas it is
// the precise boundary at which IEEE-754 doubles can no longer represent
// consecutive integers without rounding.
func TestMaxSafeJSONInt_MatchesJavaScript(t *testing.T) {
	// Number.MAX_SAFE_INTEGER in ECMAScript: 2^53 - 1 = 9007199254740991.
	const jsMaxSafeInteger uint64 = 9007199254740991
	if MaxSafeJSONInt != jsMaxSafeInteger {
		t.Errorf("MaxSafeJSONInt = %d, want %d (Number.MAX_SAFE_INTEGER = 2^53-1)",
			MaxSafeJSONInt, jsMaxSafeInteger)
	}
}

// TestMaxSafeJSONInt_BoundaryRoundTrip demonstrates the precision failure
// this constant exists to prevent: JSON-encoding a uint64 just above the
// safe range and decoding it through float64 (the path every JavaScript
// JSON.parse takes) loses the low bit.
//
// Go's json package does not decode into float64 by default when the
// target is uint64, so we simulate the JS path explicitly via a float64
// destination. The test failing would mean float64 suddenly has 64-bit
// integer precision — i.e. the universe has new physics.
func TestMaxSafeJSONInt_BoundaryRoundTrip(t *testing.T) {
	// At MaxSafeJSONInt the round-trip is exact.
	safe := MaxSafeJSONInt
	safeBytes, err := json.Marshal(safe)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var safeOut float64
	if err := json.Unmarshal(safeBytes, &safeOut); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if uint64(safeOut) != safe {
		t.Errorf("safe round-trip failed: %d -> float64 -> %d", safe, uint64(safeOut))
	}

	// One past the boundary the round-trip truncates. We pick a value
	// whose low bit is 1 so rounding to the nearest even-bit-pattern
	// double flips it.
	unsafe := MaxSafeJSONInt + 2 // 2^53 + 1 — not exactly representable as float64
	unsafeBytes, err := json.Marshal(unsafe)
	if err != nil {
		t.Fatalf("marshal unsafe: %v", err)
	}
	var unsafeOut float64
	if err := json.Unmarshal(unsafeBytes, &unsafeOut); err != nil {
		t.Fatalf("unmarshal unsafe: %v", err)
	}
	if uint64(unsafeOut) == unsafe {
		t.Errorf("expected precision loss above MaxSafeJSONInt, but "+
			"%d survived the float64 round-trip — did IEEE-754 gain a bit?",
			unsafe)
	}
}
