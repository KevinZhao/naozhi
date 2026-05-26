package cli

import "testing"

// TestProtocolLimits_RelationshipInvariants pins the cross-constant
// invariants that govern naozhi's shim-wire protocol limits. R242-ARCH-9
// (#727): the constants live in process.go, are referenced from
// process_shim_io.go / passthrough.go / process_readloop.go, and the
// rationale comments document a chain of inequalities (shim accepts
// 16 MiB; we read up to 10 MiB; we send up to 12 MiB; transient reuse
// shrinks at 256 KiB) that must hold for the whole framing model to
// stay sound. Without an executable assertion, a future tuning that
// breaks the chain (e.g. raising maxStdinLineBytes above the shim cap)
// would only surface as a connection reset under load.
//
// The original proposal in #727 was to extract a ProtocolLimits struct;
// the constants today are unexported package-locals so a struct doesn't
// add encapsulation. What WAS missing was an executable hub that names
// every related constant and the relationships between them — exactly
// what this test provides.
//
// Per-constant pinning of lineBufShrinkThreshold lives in
// process_linebuf_test.go; this file owns only the cross-constant
// relationships (the part the original proposal was complaining about).
func TestProtocolLimits_RelationshipInvariants(t *testing.T) {
	// shim accepts 16 MiB per line; we must stay below to keep headroom
	// for the shim-side ServerMsg envelope added in shim/protocol.go.
	const shimAcceptsBytes = 16 * 1024 * 1024

	if maxScannerBufBytes >= shimAcceptsBytes {
		t.Errorf("maxScannerBufBytes (%d) must stay below shim accept cap (%d) "+
			"to leave room for the shim-side ServerMsg envelope",
			maxScannerBufBytes, shimAcceptsBytes)
	}
	if maxStdinLineBytes >= shimAcceptsBytes {
		t.Errorf("maxStdinLineBytes (%d) must stay below shim accept cap (%d) "+
			"to leave room for the shim-side ClientMsg envelope",
			maxStdinLineBytes, shimAcceptsBytes)
	}

	// lineBufShrinkThreshold must sit comfortably below maxScannerBufBytes
	// so a normal (non-pathological) large event keeps its grown capacity
	// across iterations rather than triggering the shrink.
	if lineBufShrinkThreshold >= maxScannerBufBytes/8 {
		t.Errorf("lineBufShrinkThreshold (%d) is too close to maxScannerBufBytes (%d); "+
			"common large events would trigger needless shrink",
			lineBufShrinkThreshold, maxScannerBufBytes)
	}

	// Sanity: positive values; preserves the rationale that 0 (or negative
	// via misguided int cast) would degenerate into an immediate-reject
	// regression.
	if maxScannerBufBytes <= 0 || maxStdinLineBytes <= 0 || lineBufShrinkThreshold <= 0 {
		t.Fatalf("protocol limits must be positive: scanner=%d stdin=%d shrink=%d",
			maxScannerBufBytes, maxStdinLineBytes, lineBufShrinkThreshold)
	}
}
