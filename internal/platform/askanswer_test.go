package platform

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestAskAnswerPayload_WireTags pins the JSON keys: cards already posted in
// chats carry these keys, so a renamed tag would make their clicks undecodable.
func TestAskAnswerPayload_WireTags(t *testing.T) {
	t.Parallel()
	full := AskAnswerPayload{AgentID: "a", ChatType: "group", Header: "h", Kind: AskAnswerKind, Label: "l", ThreadID: "om_1", ToolUseID: "t"}
	data, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"agent_id":"a","chat_type":"group","header":"h","kind":"ask_answer","label":"l","thread_id":"om_1","tool_use_id":"t"}`
	if string(data) != want {
		t.Errorf("Marshal = %s\nwant      %s", data, want)
	}
	var back AskAnswerPayload
	if err := json.Unmarshal([]byte(want), &back); err != nil || back != full {
		t.Errorf("Unmarshal = %+v, %v; want %+v", back, err, full)
	}

	data, err = json.Marshal(AskAnswerPayload{Kind: AskAnswerKind, Label: "l"})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(data); strings.Contains(s, "agent_id") || strings.Contains(s, "chat_type") || strings.Contains(s, "thread_id") {
		t.Errorf("empty agent_id/chat_type/thread_id must be omitted: %s", s)
	}
}

func TestNewAskAnswerPayload_CapsAndWhitelist(t *testing.T) {
	t.Parallel()
	card := QuestionCard{
		ToolUseID: strings.Repeat("t", 300),
		ChatType:  "p2p",
		AgentID:   strings.Repeat("代", 300),
	}
	item := QuestionItem{Header: strings.Repeat("头", 300)}
	opt := QuestionOption{Label: strings.Repeat("标", 700), Description: "not embedded"}
	p := NewAskAnswerPayload(card, item, opt)
	if p.Kind != AskAnswerKind {
		t.Errorf("Kind = %q", p.Kind)
	}
	if p.ChatType != "" {
		t.Errorf("non-whitelisted chat type leaked: %q", p.ChatType)
	}
	for name, c := range map[string]struct {
		got string
		max int
	}{
		"tool_use_id": {p.ToolUseID, AskIDMaxRunes},
		"agent_id":    {p.AgentID, AskIDMaxRunes},
		"header":      {p.Header, AskHeaderMaxRunes},
		"label":       {p.Label, AskLabelMaxRunes},
	} {
		if n := utf8.RuneCountInString(c.got); n != c.max {
			t.Errorf("%s rune count = %d, want capped at %d", name, n, c.max)
		}
	}

	card.ChatType = "group"
	if got := NewAskAnswerPayload(card, item, opt).ChatType; got != "group" {
		t.Errorf("whitelisted chat type dropped: %q", got)
	}
}

// TestNewAskAnswerPayload_ThreadID: the card's thread round-trips through the
// button, and an id too long for the value is dropped, not clipped into the
// id of some other thread.
func TestNewAskAnswerPayload_ThreadID(t *testing.T) {
	t.Parallel()
	card := QuestionCard{ThreadID: "om_root"}
	if got := NewAskAnswerPayload(card, QuestionItem{}, QuestionOption{Label: "l"}).ThreadID; got != "om_root" {
		t.Errorf("ThreadID = %q, want om_root", got)
	}
	card.ThreadID = "om_" + strings.Repeat("x", AskIDMaxRunes)
	if got := NewAskAnswerPayload(card, QuestionItem{}, QuestionOption{Label: "l"}).ThreadID; got != "" {
		t.Errorf("over-long ThreadID = %q, want dropped", got)
	}
}

// TestNormalizeAskChatType pins the whitelist: only the two router-known
// values survive; everything else (including attacker-relayed junk) is
// dropped to "" so callers fall back to their own heuristic.
func TestNormalizeAskChatType(t *testing.T) {
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
		if got := NormalizeAskChatType(in); got != want {
			t.Errorf("NormalizeAskChatType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestComposeAskAnswerText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   AskAnswerPayload
		want string
	}{
		{"normal", AskAnswerPayload{Header: "Error style", Label: "Return an error"}, "Error style: Return an error."},
		{"no header", AskAnswerPayload{Label: "A"}, "A."},
		{"empty label", AskAnswerPayload{Header: "H"}, ""},
		{"trims spaces", AskAnswerPayload{Header: "  H  ", Label: "  L  "}, "H: L."},
		{"neutralises bidi and LS", AskAnswerPayload{Header: "H\u202e", Label: "L\u2028x"}, "H_: L_x."},
	}
	for _, tc := range cases {
		if got := ComposeAskAnswerText(tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}

	long := ComposeAskAnswerText(AskAnswerPayload{Header: strings.Repeat("h", 500), Label: strings.Repeat("l", 2000)})
	if want := strings.Repeat("h", AskHeaderMaxRunes) + ": " + strings.Repeat("l", AskLabelMaxRunes) + "."; long != want {
		t.Errorf("oversized fields not capped: len=%d, want len=%d", len(long), len(want))
	}
}

func TestRenderAskQuestionPlain(t *testing.T) {
	t.Parallel()
	got := RenderAskQuestionPlain([]QuestionItem{
		{Question: "Which?", Header: "H", Options: []QuestionOption{{Label: "A", Description: "first"}, {Label: "B"}}},
		{Question: "Then?", Options: []QuestionOption{{Label: "C"}}},
	})
	const want = "\n【H】Which?\n  1. A — first\n  2. B\n\n问题 2：Then?\n  1. C\n"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if got := RenderAskQuestionPlain(nil); got != "" {
		t.Error("no items must render empty")
	}
}
