package cli

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestPeerAndRefusalEntries: the frames CC 2.1.316 writes for a cross-session
// message (an idle session's replay with the model-facing preamble, a busy
// one's bare one), a held or dropped message and the two refusals each become
// one entry of their own kind; a replay with no peer origin and a release
// (its replay follows) stay out of the log.
func TestPeerAndRefusalEntries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line string
		want []clievent.EventEntry // nil = no entry
	}{
		{"inbound idle",
			`{"type":"user","message":{"role":"user","content":"Another Claude session sent a message:\n<cross-session-message from=\"uds:/tmp/cc-socks/1.sock\" from-name=\"p9-sender\" from-mode=\"bypass\">\nhello\n</cross-session-message>\n\nThis came from another Claude session"},"session_id":"s","parent_tool_use_id":null,"uuid":"cd336536-eb18-4dbb-88f4-3f00a55469c6","isReplay":true,"isSynthetic":true,"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/1.sock","verifiedPeerPid":1,"msg_id":"m","name":"p9-sender","fromMode":"bypass","body":"hello"}}`,
			[]clievent.EventEntry{{Type: clievent.KindPeerMessage, UUID: "cd336536eb184dbb88f43f00a55469c6", Peer: "p9-sender", PeerMode: "bypass", Summary: "hello", Detail: "hello"}}},
		{"inbound mid-turn",
			`{"type":"user","message":{"role":"user","content":"<cross-session-message from=\"uds:/tmp/cc-socks/2.sock\" from-name=\"planner\" from-mode=\"dontAsk\">\n在忙吗\n</cross-session-message>"},"uuid":"8bd2fc7b-a013-49d9-8152-22b5a1bdcc9b","isReplay":true,"isSynthetic":true,"origin":{"kind":"peer","name":"planner","fromMode":"dontAsk","body":"在忙吗"}}`,
			[]clievent.EventEntry{{Type: clievent.KindPeerMessage, UUID: "8bd2fc7ba01349d9815222b5a1bdcc9b", Peer: "planner", PeerMode: "dontAsk", Summary: "在忙吗", Detail: "在忙吗"}}},
		{"own replay",
			`{"type":"user","message":{"role":"user","content":"hi"},"uuid":"u1","isReplay":true}`,
			nil},
		{"task notification origin",
			`{"type":"user","message":{"role":"user","content":"done"},"uuid":"u2","isReplay":true,"origin":{"kind":"task-notification"}}`,
			nil},
		{"held",
			`{"type":"system","subtype":"peer_message_hold","state":"held","message_uuid":"m","lane":"socket","from":"uds:/tmp/cc-socks/3.sock","from_name":"p9-sender","cause":"explicit-setting","uuid":"u3","session_id":"s"}`,
			[]clievent.EventEntry{{Type: clievent.KindPeerHold, Peer: "p9-sender", Summary: "p9-sender", Status: "held", Reason: "explicit-setting"}}},
		{"released",
			`{"type":"system","subtype":"peer_message_hold","state":"released","message_uuid":"m","lane":"socket","from":"uds:/tmp/cc-socks/3.sock","from_name":"p9-sender","uuid":"u6","session_id":"s"}`,
			nil},
		{"dropped",
			`{"type":"system","subtype":"peer_message_hold","state":"dropped","message_uuid":"m","lane":"socket","from":"uds:/tmp/cc-socks/3.sock","from_name":"p9-sender","outcome":"expired","uuid":"u7","session_id":"s"}`,
			[]clievent.EventEntry{{Type: clievent.KindPeerHold, Peer: "p9-sender", Summary: "p9-sender", Status: "dropped", Reason: "expired"}}},
		{"permission denied",
			`{"type":"system","subtype":"permission_denied","tool_name":"Bash","tool_use_id":"toolu_2","decision_reason_type":"classifier","decision_reason":"pushes to an untrusted repo","message":"Permission to use Bash has been denied.","uuid":"u4","session_id":"s"}`,
			[]clievent.EventEntry{{Type: clievent.KindPermissionDenied, Tool: "Bash", Reason: "pushes to an untrusted repo", Summary: "Permission to use Bash has been denied.", Detail: "Permission to use Bash has been denied."}}},
		{"model refusal",
			`{"type":"system","subtype":"model_refusal_no_fallback","original_model":"claude-opus-5-5","request_id":null,"api_refusal_category":"cyber","api_refusal_explanation":null,"content":"I can't help with that.","uuid":"u5","session_id":"s"}`,
			[]clievent.EventEntry{{Type: clievent.KindModelRefusal, Reason: "cyber", Summary: "I can't help with that.", Detail: "I can't help with that."}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			evs, _, err := (&ClaudeProtocol{}).ReadEvent(tc.line)
			if err != nil || len(evs) != 1 {
				t.Fatalf("ReadEvent = %v, %v; want one event", evs, err)
			}
			got := EventEntriesFromEventAt(evs[0], 1)
			if len(got) != len(tc.want) {
				t.Fatalf("entries = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i].Type != tc.want[i].Type || got[i].UUID != tc.want[i].UUID || got[i].Peer != tc.want[i].Peer ||
					got[i].PeerMode != tc.want[i].PeerMode || got[i].Summary != tc.want[i].Summary ||
					got[i].Detail != tc.want[i].Detail || got[i].Tool != tc.want[i].Tool || got[i].Reason != tc.want[i].Reason || got[i].Status != tc.want[i].Status {
					t.Errorf("entry %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
