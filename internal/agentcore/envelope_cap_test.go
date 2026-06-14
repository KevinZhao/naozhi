package agentcore

import (
	"testing"

	"github.com/naozhi/naozhi/internal/limits"
)

// TestMaxEnvelopeLineBytes_DerivesFromSharedLineCap pins the #2084 invariant:
// the SSE envelope ceiling is the shared stream-json line cap plus a fixed
// framing-overhead headroom, NOT an independent 16MB literal. If a future
// edit re-bakes a literal here (or changes the overhead), this fails so the
// drift the shared constant was meant to prevent stays prevented.
func TestMaxEnvelopeLineBytes_DerivesFromSharedLineCap(t *testing.T) {
	const overhead = 64 << 10 // envelope + JSON-escaping headroom
	want := limits.MaxStreamJSONLine + overhead
	if MaxEnvelopeLineBytes != want {
		t.Fatalf("MaxEnvelopeLineBytes = %d, want limits.MaxStreamJSONLine(%d)+%d = %d",
			MaxEnvelopeLineBytes, limits.MaxStreamJSONLine, overhead, want)
	}
	// The envelope ceiling must exceed the base line cap (a wrapped max-size
	// line still fits), never fall below it.
	if MaxEnvelopeLineBytes <= limits.MaxStreamJSONLine {
		t.Fatalf("envelope cap %d must exceed base line cap %d",
			MaxEnvelopeLineBytes, limits.MaxStreamJSONLine)
	}
}
