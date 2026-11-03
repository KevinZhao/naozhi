package discovery

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestParseHistoryLine_PeerMessage: the transcript keeps a cross-session
// message as an idle session's user line or a busy one's queued_command
// attachment; both replay as the peer_message the live stream logged, with
// the uuid that stream gave it, and nothing else comes out of either line.
func TestParseHistoryLine_PeerMessage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, line, wantUUID string
	}{
		{"idle user line",
			`{"type":"user","message":{"role":"user","content":"Another Claude session sent a message:\n<cross-session-message from=\"uds:/tmp/cc-socks/1.sock\" from-name=\"p9-sender\" from-mode=\"bypass\">\nhello\n</cross-session-message>"},"isMeta":true,"uuid":"cd336536-eb18-4dbb-88f4-3f00a55469c6","timestamp":"2026-11-03T16:42:02.500Z","origin":{"kind":"peer","name":"p9-sender","fromMode":"bypass","body":"hello"},"turnOrigin":"peer"}`,
			"cd336536eb184dbb88f43f00a55469c6"},
		{"mid-turn attachment",
			`{"type":"attachment","attachment":{"type":"queued_command","prompt":"<cross-session-message from-name=\"p9-sender\">\nhello\n</cross-session-message>","source_uuid":"8bd2fc7b-a013-49d9-8152-22b5a1bdcc9b","origin":{"kind":"peer","name":"p9-sender","fromMode":"bypass","body":"hello"},"isMeta":true},"uuid":"aaf266c9-af6e-4188-9e13-451bd145d56d","timestamp":"2026-11-03T16:49:10.771Z"}`,
			"8bd2fc7ba01349d9815222b5a1bdcc9b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseHistoryLine([]byte(tc.line), 0, true)
			if !ok || len(got) != 1 {
				t.Fatalf("parseHistoryLine = %+v, %v; want one entry", got, ok)
			}
			e := got[0]
			if e.Type != clievent.KindPeerMessage || e.UUID != tc.wantUUID || e.Peer != "p9-sender" ||
				e.PeerMode != "bypass" || e.Detail != "hello" || e.Time <= 0 {
				t.Errorf("entry = %+v", e)
			}
		})
	}
}
