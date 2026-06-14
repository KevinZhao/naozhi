package limits

import "testing"

// TestMaxStreamJSONLine pins the shared claude stream-json line ceiling
// (#2084). The value is the upstream CLI invariant; several unrelated
// transports (node ReverseConn, upstream connector, dashboard tool-result
// file cap, agentcore SSE envelope base) bound a single frame/read/file at
// exactly this number. If the CLI cap ever changes, this is the one place to
// edit — this test documents the current value so an accidental edit is
// visible in review.
func TestMaxStreamJSONLine(t *testing.T) {
	const want = 16 * 1024 * 1024 // 16 MiB
	if MaxStreamJSONLine != want {
		t.Fatalf("MaxStreamJSONLine = %d, want %d (16 MiB CLI stdout ceiling)", MaxStreamJSONLine, want)
	}
}
