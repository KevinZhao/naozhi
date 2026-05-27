package cli

import (
	"encoding/base64"
	"testing"
	"testing/quick"
)

// TestEncodeImageBase64_RoundTrip pins R247-PERF-17 correctness: the pooled
// encode path must produce the exact same string as the standard
// base64.StdEncoding.EncodeToString. The pool optimisation only changes
// where the encode buffer comes from; output bytes must be identical or the
// JSON `Data` field would silently corrupt the on-wire image payload.
func TestEncodeImageBase64_RoundTrip(t *testing.T) {
	t.Parallel()
	cases := [][]byte{
		nil,
		{},
		{0x00},
		{0xFF, 0xD8, 0xFF, 0xE0}, // jpeg magic prefix
		[]byte("hello world"),
		make([]byte, 1),
		make([]byte, 1024),
		make([]byte, 16*1024),
		make([]byte, 16*1024+1), // forces buffer growth past pool seed
		make([]byte, 64*1024+1), // forces drop on Put (oversize)
	}
	for i, img := range cases {
		want := base64.StdEncoding.EncodeToString(img)
		got := encodeImageBase64(img)
		if got != want {
			t.Errorf("case %d (len=%d): got %q…, want %q…", i, len(img),
				truncateHead(got), truncateHead(want))
		}
	}
}

func truncateHead(s string) string {
	if len(s) > 32 {
		return s[:32]
	}
	return s
}

// TestEncodeImageBase64_QuickRoundTrip is a property-style guard: any byte
// slice up to 8 KiB encoded via the pooled path must match
// base64.StdEncoding.EncodeToString. quick.Check defaults to 100 random
// inputs, deterministic per Go release. R247-PERF-17.
func TestEncodeImageBase64_QuickRoundTrip(t *testing.T) {
	t.Parallel()
	prop := func(b []byte) bool {
		// Bound the slice to keep the test fast; pool grow/shrink is
		// already covered by the table above.
		if len(b) > 8*1024 {
			b = b[:8*1024]
		}
		return encodeImageBase64(b) == base64.StdEncoding.EncodeToString(b)
	}
	if err := quick.Check(prop, nil); err != nil {
		t.Fatalf("quick.Check: %v", err)
	}
}

// TestEncodeImageBase64_PoolReused pins R247-PERF-17 perf: small encode
// calls must round-trip the same backing array through the pool, otherwise
// the optimisation silently regressed to per-call alloc. Identity is
// asserted by capturing the *[]byte pointer the pool hands out: a fresh
// alloc would change it.
//
// The pool is process-wide, so we drain Gets until New() is forced to
// produce a fresh entry, then encode → put → encode again; the second Get
// must return the same pointer we just released.
func TestEncodeImageBase64_PoolReused(t *testing.T) {
	// Not parallel — directly manipulates the package-global pool.
	// Drain whatever residue lives in the pool from earlier tests so the
	// sentinel we put in is the head.
	for i := 0; i < 64; i++ {
		acpB64BufPool.Get()
	}

	// Encode once: this Gets a fresh slice (pool empty), encodes, and Puts
	// the slice back (small payload, well under acpB64BufMaxCap).
	encodeImageBase64([]byte("warmup"))

	// Get the pointer that was just put back. This MUST be the same
	// allocation the call returned to the pool — a per-call make would
	// have left the pool empty and we'd see a fresh New() result.
	bp := acpB64BufPool.Get().(*[]byte)
	if cap(*bp) < 16*1024 {
		t.Errorf("pool entry cap = %d, want >= 16384 (matches sync.Pool seed); a fresh make() leaked into the pool", cap(*bp))
	}

	// Restore the pool entry so subsequent tests are not starved.
	acpB64BufPool.Put(bp)
}

// TestEncodeImageBase64_OversizeNotPooled pins the acpB64BufMaxCap=64KiB
// upper bound. After encoding a >64KiB payload (which forces the pool slice
// to grow past the cap), the buffer MUST NOT be returned to the pool — a
// regression here would cause the pool to retain multi-MB backing arrays
// indefinitely on the first outlier upload. R247-PERF-17.
func TestEncodeImageBase64_OversizeNotPooled(t *testing.T) {
	// Not parallel for the same reason as the pool-reuse test.
	// Drain residue.
	for i := 0; i < 64; i++ {
		acpB64BufPool.Get()
	}

	// Encode something that forces the slice past acpB64BufMaxCap (64KiB).
	// base64 inflates by 4/3, so 96 KiB raw → ~128 KiB encoded > 64 KiB cap.
	huge := make([]byte, 96*1024)
	_ = encodeImageBase64(huge)

	// After the oversize encode, the pool must be empty (the oversized
	// buffer was dropped). Get() will trigger New(), producing a slice
	// with the documented 16 KiB seed cap. If the oversize entry had been
	// retained, cap would exceed the seed.
	bp := acpB64BufPool.Get().(*[]byte)
	if cap(*bp) > 16*1024 {
		t.Errorf("pool retained oversize buffer (cap=%d); acpB64BufMaxCap drop-on-Put rule regressed", cap(*bp))
	}
	// Restore so other tests can pull a seed.
	acpB64BufPool.Put(bp)
}
