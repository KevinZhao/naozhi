package clievent_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestEventEntry_JSONShape pins the wire shape against persisted records.
// Any field rename / json tag drift would silently break replay of
// existing <dataDir>/sessions/*.jsonl files; spot-check the most-load-
// bearing fields here so the cron CI catches it.
func TestEventEntry_JSONShape(t *testing.T) {
	e := clievent.EventEntry{
		UUID:    "u1",
		Time:    1700000000000,
		Type:    "user",
		Summary: "hello",
		AskQuestion: &clievent.AskQuestion{
			ToolUseID: "tu1",
			Items: []clievent.AskQuestionItem{{
				Question: "q?",
				Options:  []clievent.AskQuestionOpt{{Label: "yes"}},
			}},
		},
		ToolCall: &clievent.ToolCall{ID: "tc1", Status: "completed"},
	}
	buf, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wantSubs := []string{
		`"uuid":"u1"`,
		`"time":1700000000000`,
		`"type":"user"`,
		`"summary":"hello"`,
		`"ask_question":`,
		`"tool_use_id":"tu1"`,
		`"tool_call":`,
		`"id":"tc1"`,
		`"status":"completed"`,
	}
	got := string(buf)
	for _, s := range wantSubs {
		if !strings.Contains(got, s) {
			t.Errorf("JSON missing %q\nfull: %s", s, got)
		}
	}
}
