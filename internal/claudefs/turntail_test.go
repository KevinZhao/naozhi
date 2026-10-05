package claudefs

import (
	"path/filepath"
	"strings"
	"testing"
)

// Record shapes as the CLI writes them (field order and the metadata lines
// that trail an ended turn are taken from real transcripts).
const (
	ttUserPrompt = `{"parentUuid":null,"isSidechain":false,"promptId":"p1","type":"user","message":{"role":"user","content":"go"},"uuid":"u1","timestamp":"2026-10-05T01:00:00.000Z","sessionId":"s"}`
	ttToolResult = `{"parentUuid":"a1","isSidechain":false,"promptId":"p1","type":"user","message":{"role":"user","content":[{"tool_use_id":"t1","type":"tool_result","content":"ok"}]},"uuid":"u2","sessionId":"s"}`
	ttToolUse    = `{"parentUuid":"u1","isSidechain":false,"message":{"model":"m","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}],"stop_reason":"tool_use"},"type":"assistant","uuid":"a1","sessionId":"s"}`
	ttEndTurn    = `{"parentUuid":"u2","isSidechain":false,"message":{"model":"m","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"},"type":"assistant","uuid":"a2","sessionId":"s"}`
	ttAttachment = `{"parentUuid":"a2","isSidechain":false,"attachment":{"type":"prompt_snapshot"},"type":"attachment","uuid":"x1","sessionId":"s"}`
	ttTurnDur    = `{"parentUuid":"a2","isSidechain":false,"type":"system","subtype":"turn_duration","durationMs":5,"uuid":"x2","sessionId":"s"}`
	ttLastPrompt = `{"type":"last-prompt","lastPrompt":"go","leafUuid":"a2","sessionId":"s"}`
	ttCostState  = `{"type":"cost-state","sessionId":"s","totalCostUSD":0.5,"totalAPIDuration":10}`
	ttSidechain  = `{"parentUuid":"z","isSidechain":true,"message":{"role":"assistant","content":[{"type":"tool_use","id":"t9","name":"Bash","input":{}}],"stop_reason":"tool_use"},"type":"assistant","uuid":"a9","sessionId":"s"}`
)

func ttLines(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

// bulk is ~size bytes of finished tool rounds, so a tail fixture is a
// realistically large file and not one the first window already covers.
func bulk(size int) string {
	var b strings.Builder
	for b.Len() < size {
		b.WriteString(ttLines(ttToolUse, ttToolResult))
	}
	return b.String()
}

// assistantWithText is an end_turn record padded to roughly n bytes.
func assistantWithText(n int, stop string) string {
	return `{"parentUuid":"u2","isSidechain":false,"message":{"role":"assistant","content":[{"type":"text","text":"` +
		strings.Repeat("x", n) + `"}],"stop_reason":"` + stop + `"},"type":"assistant","uuid":"a3","sessionId":"s"}`
}

func TestTranscriptTurnEnded(t *testing.T) {
	t.Parallel()
	const fiveMB = 5 << 20
	cases := []struct {
		name string
		body string
		want bool
	}{
		// 5 MiB: a size cap mistaken for the read window refuses this file
		// and reports "still running", the case the fix exists for.
		{"5MiB ending in end_turn plus metadata", bulk(fiveMB) + ttLines(ttEndTurn, ttAttachment, ttTurnDur, ttLastPrompt, ttCostState), true},
		{"5MiB ending in tool_use", bulk(fiveMB) + ttLines(ttUserPrompt, ttToolUse), false},
		{"tool_result last", ttLines(ttUserPrompt, ttToolUse, ttToolResult, ttAttachment), false},
		{"new prompt after an ended turn", ttLines(ttEndTurn, ttLastPrompt, ttUserPrompt), false},
		{"sidechain records do not count", ttLines(ttEndTurn, ttSidechain, ttSidechain), true},
		{"stop_sequence (synthetic API error) ends a turn", ttLines(ttUserPrompt, assistantWithText(10, "stop_sequence")), true},
		{"refusal ends a turn", ttLines(ttUserPrompt, assistantWithText(10, "refusal")), true},
		{"missing stop_reason does not", ttLines(ttUserPrompt, strings.Replace(ttEndTurn, `,"stop_reason":"end_turn"`, "", 1)), false},
		{"result record", ttLines(ttToolUse, `{"type":"result","subtype":"success","sessionId":"s"}`), true},
		{"single-line file: the first line is whole", ttEndTurn + "\n", true},
		{"torn last line is skipped", ttLines(ttEndTurn) + ttToolUse[:40], true},
		{"metadata only", ttLines(ttLastPrompt, ttCostState), false},
		{"empty file", "", false},
		// The last record alone exceeds the 64 KiB window: only the widened
		// window holds it whole.
		{"200KiB last record", bulk(2<<20) + ttLines(assistantWithText(200<<10, "end_turn"), ttLastPrompt, ttCostState), true},
		{"200KiB last record, tool_use", bulk(2<<20) + ttLines(assistantWithText(200<<10, "tool_use"), ttLastPrompt), false},
		// Past the widest window there is nothing to decide on: report running.
		{"last record over 1MiB", bulk(2<<20) + ttLines(assistantWithText(1100<<10, "end_turn"), ttLastPrompt), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := TranscriptTurnEnded(writeTranscript(t, tc.body))
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Errorf("TranscriptTurnEnded = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTranscriptTurnEnded_Unreadable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if ended, err := TranscriptTurnEnded(filepath.Join(dir, "absent.jsonl")); err == nil || ended {
		t.Errorf("absent file = %v, %v; want false and an error", ended, err)
	}
	if ended, err := TranscriptTurnEnded(dir); err == nil || ended {
		t.Errorf("directory = %v, %v; want false and an error", ended, err)
	}
}
