package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// askUserQuestionLine is a captured fixture from the live validation harness
// (test/e2e/askuser/out/aq1_aq2_stream.log, claude CLI 2.1.132).
// Trimmed non-essential fields (model id, signatures, session_id) for clarity.
const askUserQuestionLine = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_bdrk_013M2ngbuySW8zMNotxACh9U","name":"AskUserQuestion","input":{"questions":[{"question":"Which error handling approach do you want to use?","header":"Error style","multiSelect":false,"options":[{"label":"Return an error","description":"Propagate the error to the caller."},{"label":"Log and continue","description":"Log and keep running."},{"label":"Panic","description":"Crash immediately."}]},{"question":"Which function do you want to add error handling to?","header":"Target","multiSelect":false,"options":[{"label":"I'll paste the code","description":"You'll share the snippet."},{"label":"Point me to a file","description":"You'll give a path."}]}]}}]},"session_id":"s1","uuid":"u1"}`

func TestClaudeProtocol_ReadEvent_AskUserQuestion(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	ev, done, err := readOne(t, p, askUserQuestionLine)
	if err != nil {
		t.Fatalf("ReadEvent err=%v", err)
	}
	if done {
		t.Fatal("assistant tool_use should not be done")
	}
	if ev.AskQuestion == nil {
		t.Fatal("expected AskQuestion to be populated on tool_use=AskUserQuestion")
	}
	aq := ev.AskQuestion
	if aq.ToolUseID != "toolu_bdrk_013M2ngbuySW8zMNotxACh9U" {
		t.Errorf("ToolUseID = %q, want toolu_bdrk_013M2ngbuySW8zMNotxACh9U", aq.ToolUseID)
	}
	if len(aq.Items) != 2 {
		t.Fatalf("Items count = %d, want 2", len(aq.Items))
	}
	if aq.Items[0].Header != "Error style" {
		t.Errorf("Items[0].Header = %q, want Error style", aq.Items[0].Header)
	}
	if len(aq.Items[0].Options) != 3 {
		t.Errorf("Items[0].Options count = %d, want 3", len(aq.Items[0].Options))
	}
	if aq.Items[0].Options[0].Label != "Return an error" {
		t.Errorf("Items[0].Options[0].Label = %q", aq.Items[0].Options[0].Label)
	}
	if aq.Items[0].MultiSelect {
		t.Error("Items[0].MultiSelect should be false (fixture says false)")
	}
	// Ensure the assistant event itself is still returned intact — dispatch still
	// needs the tool_use bubble so the existing transcript rendering works.
	if ev.Message == nil || len(ev.Message.Content) == 0 {
		t.Fatal("original assistant content must be preserved")
	}
	if ev.Message.Content[0].Type != "tool_use" || ev.Message.Content[0].Name != "AskUserQuestion" {
		t.Error("original tool_use block must remain addressable")
	}
}

func TestClaudeProtocol_ReadEvent_NonAskQuestionUntouched(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	// A bare text assistant message — no AskUserQuestion tool_use.
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`
	ev, _, err := readOne(t, p, line)
	if err != nil {
		t.Fatalf("ReadEvent err=%v", err)
	}
	if ev.AskQuestion != nil {
		t.Error("AskQuestion should be nil when no AskUserQuestion tool_use present")
	}
}

// TestClaudeProtocol_ReadEvent_FastPathSkipsParseForBashToolUse pins
// R234-PERF-16 (#1008): an assistant event carrying ONLY non-AQ tool_uses
// (e.g. Bash) must produce AskQuestion=nil — the substring fast path
// short-circuits before extractAskQuestion runs at all. The fixture
// deliberately omits the literal "AskUserQuestion" substring anywhere
// in the line so the strings.Contains gate evaluates to false.
func TestClaudeProtocol_ReadEvent_FastPathSkipsParseForBashToolUse(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_x","name":"Bash","input":{"command":"ls"}}]}}`
	ev, _, err := readOne(t, p, line)
	if err != nil {
		t.Fatalf("ReadEvent err=%v", err)
	}
	if ev.AskQuestion != nil {
		t.Errorf("fast-path failed: AskQuestion=%+v on Bash tool_use", ev.AskQuestion)
	}
	// Verify the original tool_use bubble still flows through.
	if ev.Message == nil || len(ev.Message.Content) != 1 ||
		ev.Message.Content[0].Name != "Bash" {
		t.Error("Bash tool_use block must remain intact after fast-path skip")
	}
}

func TestExtractAskQuestion_ReturnsNilOnMalformed(t *testing.T) {
	t.Parallel()
	// tool_use with name=AskUserQuestion but malformed input (not JSON object)
	blocks := []ContentBlock{{
		Type: "tool_use", Name: "AskUserQuestion", ID: "toolu_xxx",
		Input: json.RawMessage(`"not-an-object"`),
	}}
	if aq := extractAskQuestion(blocks); aq != nil {
		t.Errorf("expected nil on malformed input, got %+v", aq)
	}
	// Empty questions array is also a no-op — nothing to render.
	blocks = []ContentBlock{{
		Type: "tool_use", Name: "AskUserQuestion", ID: "toolu_xxx",
		Input: json.RawMessage(`{"questions":[]}`),
	}}
	if aq := extractAskQuestion(blocks); aq != nil {
		t.Error("expected nil on empty questions, got non-nil")
	}
}

func TestExtractAskQuestion_IgnoresNonAskToolUse(t *testing.T) {
	t.Parallel()
	blocks := []ContentBlock{
		{Type: "tool_use", Name: "Bash", ID: "t1", Input: json.RawMessage(`{"command":"ls"}`)},
		{Type: "text", Text: "hello"},
	}
	if aq := extractAskQuestion(blocks); aq != nil {
		t.Errorf("expected nil when no AskUserQuestion present, got %+v", aq)
	}
}

func TestEventEntriesFromEventAt_AskQuestionEntry(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	ev, _, err := readOne(t, p, askUserQuestionLine)
	if err != nil {
		t.Fatalf("ReadEvent err=%v", err)
	}
	entries := EventEntriesFromEventAt(ev, 1000)
	// Expect at least one tool_use entry AND one ask_question entry.
	var sawToolUse, sawAskQuestion bool
	for _, e := range entries {
		switch e.Type {
		case "tool_use":
			if e.Tool == "AskUserQuestion" {
				sawToolUse = true
			}
		case "ask_question":
			sawAskQuestion = true
			if e.AskQuestion == nil {
				t.Error("ask_question entry missing AskQuestion payload")
			}
			if e.ToolUseID == "" {
				t.Error("ask_question entry missing ToolUseID")
			}
			if !strings.Contains(e.Summary, "error handling") {
				t.Errorf("ask_question Summary unexpected: %q", e.Summary)
			}
		}
	}
	if !sawToolUse {
		t.Error("expected a tool_use entry for AskUserQuestion")
	}
	if !sawAskQuestion {
		t.Error("expected a synthesised ask_question entry")
	}
}

// JSON round-trip: ensure AskQuestion serialises cleanly through EventEntry for
// the WS path (the dashboard receives EventEntry as JSON).
func TestEventEntry_AskQuestion_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	e := EventEntry{
		Time: 42,
		Type: "ask_question",
		Tool: "AskUserQuestion",
		AskQuestion: &AskQuestion{
			ToolUseID: "toolu_abc",
			Items: []AskQuestionItem{{
				Question: "q?", Header: "H",
				Options: []AskQuestionOpt{{Label: "A"}, {Label: "B"}},
			}},
		},
	}
	buf, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal err=%v", err)
	}
	var got EventEntry
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("Unmarshal err=%v", err)
	}
	if got.AskQuestion == nil {
		t.Fatal("AskQuestion dropped through round-trip")
	}
	if got.AskQuestion.ToolUseID != "toolu_abc" {
		t.Errorf("ToolUseID drift: got %q", got.AskQuestion.ToolUseID)
	}
	if len(got.AskQuestion.Items) != 1 || len(got.AskQuestion.Items[0].Options) != 2 {
		t.Errorf("Items/Options drift: %+v", got.AskQuestion.Items)
	}
}
