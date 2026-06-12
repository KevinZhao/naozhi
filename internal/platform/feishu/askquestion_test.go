package feishu

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/platform"
)

func TestBuildQuestionCardJSON_Shape(t *testing.T) {
	t.Parallel()
	card := platform.QuestionCard{
		ToolUseID: "toolu_abc",
		Items: []platform.QuestionItem{{
			Question: "Which approach?",
			Header:   "Error style",
			Options: []platform.QuestionOption{
				{Label: "Return an error", Description: "idiomatic Go"},
				{Label: "Panic", Description: "unrecoverable"},
			},
		}},
	}
	data, err := buildQuestionCardJSON(card)
	if err != nil {
		t.Fatalf("buildQuestionCardJSON err=%v", err)
	}
	// Validate it's a well-formed JSON object with schema 2.0.
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal err=%v, raw=%s", err, string(data))
	}
	if got["schema"] != "2.0" {
		t.Errorf("schema = %v, want 2.0", got["schema"])
	}
	// Cheap structural checks: the value object must be embedded in each button.
	s := string(data)
	if !strings.Contains(s, `"tool_use_id":"toolu_abc"`) {
		t.Error("value.tool_use_id not embedded in card")
	}
	// session_key was intentionally removed (security-review: dead field
	// widened attack surface). The card payload must NOT embed it.
	if strings.Contains(s, `"session_key"`) {
		t.Error("value.session_key should not be embedded — was removed for security")
	}
	if !strings.Contains(s, `"kind":"ask_answer"`) {
		t.Error("value.kind not embedded in card")
	}
	if !strings.Contains(s, "Return an error") {
		t.Error("option label missing from card")
	}
	// Description should be joined to the label with em dash in the button text.
	if !strings.Contains(s, "Return an error — idiomatic Go") {
		t.Errorf("option description not joined: %s", s)
	}
}

func TestBuildMultiQuestionMarkdownCardJSON(t *testing.T) {
	t.Parallel()
	card := platform.QuestionCard{
		ToolUseID: "toolu_abc",
		Items: []platform.QuestionItem{
			{
				Question: "Which approach?",
				Header:   "Error style",
				Options: []platform.QuestionOption{
					{Label: "Return an error", Description: "idiomatic"},
					{Label: "Panic"},
				},
			},
			{
				Question: "Target?",
				Header:   "Target",
				Options:  []platform.QuestionOption{{Label: "I'll paste code"}, {Label: "Point me to file"}},
			},
		},
	}
	data, err := buildMultiQuestionMarkdownCardJSON(card)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal err=%v", err)
	}
	if got["schema"] != "2.0" {
		t.Errorf("schema = %v", got["schema"])
	}
	// Walk to the markdown content and assert both questions appear.
	body := got["body"].(map[string]any)
	elems := body["elements"].([]any)
	if len(elems) != 1 {
		t.Fatalf("expected 1 element (markdown only — no action module), got %d", len(elems))
	}
	md := elems[0].(map[string]any)
	if md["tag"] != "markdown" {
		t.Errorf("expected markdown tag, got %v", md["tag"])
	}
	content := md["content"].(string)
	mustContain := []string{
		"Error style", "Target",
		"Return an error", "Panic", "I'll paste code", "Point me to file",
		"一次回复全部", // the reply-together prompt
		"回复示例",   // example reply
	}
	for _, s := range mustContain {
		if !strings.Contains(content, s) {
			t.Errorf("multi-question card missing %q", s)
		}
	}
}

// Single-question cards must keep the action module so one click is the
// complete answer.
func TestBuildQuestionCard_SingleQuestionKeepsButtons(t *testing.T) {
	t.Parallel()
	card := platform.QuestionCard{
		ToolUseID: "t1",
		Items: []platform.QuestionItem{{
			Question: "q",
			Header:   "H",
			Options:  []platform.QuestionOption{{Label: "A"}, {Label: "B"}},
		}},
	}
	data, err := buildQuestionCardJSON(card)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	s := string(data)
	if !strings.Contains(s, `"tag":"button"`) {
		t.Errorf("single-question card missing action buttons: %s", s)
	}
}

func TestBuildQuestionCardJSON_LongLabelTrimmed(t *testing.T) {
	t.Parallel()
	// Use distinct characters for the label vs description so we can assert on
	// the button display text only. The value.label still carries the full
	// string — that's intentional so the composed answer is not truncated.
	card := platform.QuestionCard{
		ToolUseID: "t1",
		Items: []platform.QuestionItem{{
			Question: "q",
			Header:   "H",
			Options:  []platform.QuestionOption{{Label: "L", Description: strings.Repeat("d", 200)}},
		}},
	}
	data, err := buildQuestionCardJSON(card)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	// Decode and walk to find every "content" inside action.text — which is
	// the display string we clip. 100 is the cap; allow a couple chars of
	// slack for the "L — " prefix.
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal err=%v", err)
	}
	maxRuneLen := 0
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if tag, _ := t["tag"].(string); tag == "button" {
				if textMap, ok := t["text"].(map[string]any); ok {
					// Rune count is what matters — TruncateRunesNoEllipsis clips by
					// rune, and a byte-length assertion would spuriously fail on
					// multi-byte em dashes present in the "Label — Desc" join.
					if c, _ := textMap["content"].(string); utf8.RuneCountInString(c) > maxRuneLen {
						maxRuneLen = utf8.RuneCountInString(c)
					}
				}
			}
			for _, vv := range t {
				walk(vv)
			}
		case []any:
			for _, vv := range t {
				walk(vv)
			}
		}
	}
	walk(parsed)
	if maxRuneLen == 0 {
		t.Fatal("could not find any button text content")
	}
	if maxRuneLen > 100 {
		t.Errorf("button text rune count = %d, want <=100", maxRuneLen)
	}
}

func TestComposeAskAnswerText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   cardActionPayload
		want string
	}{
		{"normal", cardActionPayload{Header: "Error style", Label: "Return an error"}, "Error style: Return an error."},
		{"no header", cardActionPayload{Label: "A"}, "A."},
		{"empty label", cardActionPayload{Header: "H"}, ""},
		{"trims spaces", cardActionPayload{Header: "  H  ", Label: "  L  "}, "H: L."},
	}
	for _, tc := range cases {
		if got := composeAskAnswerText(tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDispatchCardAction_RoutesAsMessage(t *testing.T) {
	t.Parallel()
	f := &Feishu{}
	var called atomic.Int32
	var got platform.IncomingMessage
	handler := func(_ context.Context, m platform.IncomingMessage) {
		called.Add(1)
		got = m
	}
	payload := cardActionPayload{
		Kind:      "ask_answer",
		ToolUseID: "toolu_xyz",
		Header:    "Error style",
		Label:     "Return an error",
	}
	// messageID is left empty intentionally — populating it would trigger the
	// background EditMessage call, which requires a configured Feishu client
	// (token cache + stopCtx). The card-edit path is cosmetic polish and is
	// exercised via integration harness, not here.
	f.dispatchCardAction(context.Background(), payload, "oc_123", "", "group", "ou_user", handler)
	if called.Load() != 1 {
		t.Fatalf("handler called %d times, want 1", called.Load())
	}
	if got.Text != "Error style: Return an error." {
		t.Errorf("message text = %q", got.Text)
	}
	if got.ChatID != "oc_123" || got.UserID != "ou_user" || got.ChatType != "group" {
		t.Errorf("chat routing drift: %+v", got)
	}
	if !got.MentionMe {
		t.Error("card click should force MentionMe=true so group dispatch fires")
	}
}

func TestDispatchCardAction_IgnoresUnknownKind(t *testing.T) {
	t.Parallel()
	f := &Feishu{}
	var called atomic.Int32
	handler := func(_ context.Context, _ platform.IncomingMessage) { called.Add(1) }
	f.dispatchCardAction(context.Background(),
		cardActionPayload{Kind: "something_else", Label: "X"},
		"oc_1", "om_1", "direct", "ou_1", handler)
	if called.Load() != 0 {
		t.Errorf("handler should not fire on unknown kind, got %d", called.Load())
	}
}

// TestNormalizeCardChatType pins the whitelist: only the two router-known
// values survive; everything else (including attacker-relayed junk) is
// dropped to "" so callers fall back to their own heuristic.
func TestNormalizeCardChatType(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"direct": "direct",
		"group":  "group",
		"":       "",
		"p2p":    "", // Feishu's wire value for 1:1 is normalised upstream; raw form rejected here
		"GROUP":  "",
		"../etc": "",
	}
	for in, want := range cases {
		if got := normalizeCardChatType(in); got != want {
			t.Errorf("normalizeCardChatType(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildQuestionCardJSON_EmbedsChatType verifies the originating chat type
// rides in the button value so the WS card-action path can route the answer
// back to the same session key (regression for the 1:1-misrouted-as-group bug
// where every "oc_" prefix was treated as group).
func TestBuildQuestionCardJSON_EmbedsChatType(t *testing.T) {
	t.Parallel()
	card := platform.QuestionCard{
		ToolUseID: "t1",
		ChatType:  "direct",
		Items: []platform.QuestionItem{{
			Question: "q",
			Header:   "H",
			Options:  []platform.QuestionOption{{Label: "A"}},
		}},
	}
	data, err := buildQuestionCardJSON(card)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(string(data), `"chat_type":"direct"`) {
		t.Errorf("card value missing chat_type:direct: %s", data)
	}
}

// TestBuildQuestionCardJSON_OmitsUnknownChatType ensures a non-whitelisted
// chat type is not propagated into the button value (defence in depth — the
// value round-trips through Feishu and back into the session key).
func TestBuildQuestionCardJSON_OmitsUnknownChatType(t *testing.T) {
	t.Parallel()
	card := platform.QuestionCard{
		ToolUseID: "t1",
		ChatType:  "p2p", // not a router value; must be dropped
		Items: []platform.QuestionItem{{
			Question: "q",
			Options:  []platform.QuestionOption{{Label: "A"}},
		}},
	}
	data, err := buildQuestionCardJSON(card)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(string(data), `"chat_type"`) {
		t.Errorf("non-whitelisted chat_type leaked into card: %s", data)
	}
}

// TestDispatchCardAction_DirectChatTypeRoutesToDirect is the core regression:
// a 1:1 card click whose value carries chat_type=direct must route to a
// direct session even though the chat_id is an "oc_" prefix (Feishu p2p chats
// use oc_ open_chat_ids). The WS handler resolves chatType from the value and
// passes it here; this asserts the resulting IncomingMessage stays "direct".
func TestDispatchCardAction_DirectChatTypeRoutesToDirect(t *testing.T) {
	t.Parallel()
	f := &Feishu{}
	var got platform.IncomingMessage
	var called atomic.Int32
	handler := func(_ context.Context, m platform.IncomingMessage) {
		called.Add(1)
		got = m
	}
	// chatID is an oc_ prefix (a real 1:1 open_chat_id) but the value-derived
	// chatType is "direct" — the bug was treating this as "group".
	f.dispatchCardAction(context.Background(),
		cardActionPayload{Kind: "ask_answer", ToolUseID: "t1", Header: "H", Label: "L", ChatType: "direct"},
		"oc_p2p_chat", "", "direct", "ou_user", handler)
	if called.Load() != 1 {
		t.Fatalf("handler called %d times, want 1", called.Load())
	}
	if got.ChatType != "direct" {
		t.Errorf("1:1 card answer routed as %q, want direct (oc_ prefix must not force group)", got.ChatType)
	}
}

func TestHandleCardActionWebhook_ParsesShape(t *testing.T) {
	t.Parallel()
	f := &Feishu{}
	var called atomic.Int32
	var got platform.IncomingMessage
	handler := func(_ context.Context, m platform.IncomingMessage) {
		called.Add(1)
		got = m
	}
	// open_message_id intentionally empty — a populated id would spawn an
	// async EditMessage that requires a configured Feishu client.
	raw := json.RawMessage(`{
      "action":{"value":{"kind":"ask_answer","tool_use_id":"t1","session_key":"k","header":"H","label":"L"}},
      "open_chat_id":"oc_abc",
      "chat_type":"group",
      "operator":{"open_id":"ou_operator"}
    }`)
	f.handleCardActionWebhook(context.Background(), raw, handler)
	if called.Load() != 1 {
		t.Fatalf("handler called %d times, want 1", called.Load())
	}
	if got.Text != "H: L." {
		t.Errorf("text = %q", got.Text)
	}
	if got.ChatID != "oc_abc" || got.UserID != "ou_operator" {
		t.Errorf("routing fields drift: %+v", got)
	}
	// We populated open_message_id in the webhook payload; the dispatch path
	// spawns an async EditMessage which needs a fully-wired Feishu. Wait for
	// the synthetic handler then exit — the edit goroutine will fail quietly
	// via its sanitised debug log (see askquestion.go). The deferred SDK edit
	// is a best-effort polish, not behaviour we assert here.
}

// TestHandleCardActionWebhook_V2ContextNesting pins #2006: the v2
// card.action.trigger envelope nests open_chat_id/open_message_id under a
// `context` object and carries neither at top level. Without the context
// fallback the answer routes into a phantom session with an empty chat id.
func TestHandleCardActionWebhook_V2ContextNesting(t *testing.T) {
	t.Parallel()
	f := &Feishu{}
	var called atomic.Int32
	var got platform.IncomingMessage
	handler := func(_ context.Context, m platform.IncomingMessage) {
		called.Add(1)
		got = m
	}
	// open_message_id intentionally empty (in context) — a populated id would
	// spawn an async EditMessage needing a configured Feishu client.
	raw := json.RawMessage(`{
      "action":{"value":{"kind":"ask_answer","tool_use_id":"t1","header":"H","label":"L"}},
      "context":{"open_chat_id":"oc_v2chat"},
      "operator":{"open_id":"ou_operator"}
    }`)
	f.handleCardActionWebhook(context.Background(), raw, handler)
	if called.Load() != 1 {
		t.Fatalf("handler called %d times, want 1", called.Load())
	}
	if got.ChatID != "oc_v2chat" {
		t.Errorf("ChatID = %q, want oc_v2chat (context fallback failed)", got.ChatID)
	}
	if got.UserID != "ou_operator" {
		t.Errorf("UserID = %q, want ou_operator", got.UserID)
	}
}

// TestDispatchCardAction_FallsBackToValueChatType pins #2007: when the
// envelope chat_type is empty (v2 card.action.trigger has none), the chat type
// must fall back to the whitelisted value embedded in the button so a group
// card answer routes back to the group session instead of a phantom direct one.
func TestDispatchCardAction_FallsBackToValueChatType(t *testing.T) {
	t.Parallel()
	f := &Feishu{}
	var got platform.IncomingMessage
	handler := func(_ context.Context, m platform.IncomingMessage) { got = m }
	// Envelope chatType empty; value carries the originating "group".
	f.dispatchCardAction(context.Background(),
		cardActionPayload{Kind: "ask_answer", Label: "L", ChatType: "group"},
		"oc_grp", "", "", "ou_user", handler)
	if got.ChatType != "group" {
		t.Errorf("ChatType = %q, want group (value fallback failed)", got.ChatType)
	}
}
