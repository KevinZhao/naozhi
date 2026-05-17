package cli

import (
	"bytes"
	"testing"
)

// TestShimPingBytes_MatchesEncoder pins the static heartbeat payload
// against the result of going through the regular encodeShimMsg path.
// If a future change to shimClientMsg's JSON shape (rename field,
// reorder, add tag) silently breaks the wire format, this test fails
// before the heartbeat reaches a real shim. R222-PERF-14.
func TestShimPingBytes_MatchesEncoder(t *testing.T) {
	se, err := encodeShimMsg(shimClientMsg{Type: "ping"})
	if err != nil {
		t.Fatalf("encodeShimMsg: %v", err)
	}
	defer returnShimSendEnc(se)
	got := se.buf.Bytes()
	if !bytes.Equal(got, shimPingBytes) {
		t.Fatalf("shimPingBytes drift\n want %q\n got  %q", shimPingBytes, got)
	}
}
