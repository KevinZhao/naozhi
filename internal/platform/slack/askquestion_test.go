package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/slack-go/slack"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// apiCall is one form-encoded Web API request the fake recorded.
type apiCall struct {
	method string
	form   url.Values
}

// fakeWebAPI answers chat.postMessage and chat.update and records each call.
type fakeWebAPI struct {
	mu    sync.Mutex
	calls []apiCall
}

func (f *fakeWebAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		method := strings.TrimPrefix(r.URL.Path, "/")
		f.mu.Lock()
		f.calls = append(f.calls, apiCall{method: method, form: r.PostForm})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "channel": r.PostForm.Get("channel"), "ts": "1700000000.000100",
		})
	})
}

func (f *fakeWebAPI) recorded() []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]apiCall(nil), f.calls...)
}

// newAPIAdapter returns a Slack whose Web API goes to a fakeWebAPI.
func newAPIAdapter(t *testing.T) (*Slack, *fakeWebAPI) {
	t.Helper()
	f := &fakeWebAPI{}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	s.api = slack.New("xoxb-test", slack.OptionHTTPClient(slackHTTPClient), slack.OptionAPIURL(srv.URL+"/"))
	return s, f
}

// blockJSON is the subset of Block Kit the card tests look at.
type blockJSON struct {
	Type     string `json:"type"`
	Text     *struct{ Type, Text string }
	Elements []elementJSON `json:"elements"`
}

// elementJSON is a context element (a text object, text is a string) or a
// button (text is a nested text object).
type elementJSON struct {
	Type     string          `json:"type"`
	ActionID string          `json:"action_id"`
	Value    string          `json:"value"`
	RawText  json.RawMessage `json:"text"`
}

func (e elementJSON) text() string {
	var s string
	if json.Unmarshal(e.RawText, &s) == nil {
		return s
	}
	var obj struct{ Text string }
	_ = json.Unmarshal(e.RawText, &obj)
	return obj.Text
}

func decodeBlocks(t *testing.T, raw string) []blockJSON {
	t.Helper()
	var out []blockJSON
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode blocks %s: %v", raw, err)
	}
	return out
}

func renderBlocks(t *testing.T, card platform.QuestionCard) ([]blockJSON, string) {
	t.Helper()
	blocks, text, err := buildQuestionBlocks(card)
	if err != nil {
		t.Fatalf("buildQuestionBlocks: %v", err)
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	return decodeBlocks(t, string(raw)), text
}

func blockTypes(blocks []blockJSON) string {
	types := make([]string, len(blocks))
	for i, b := range blocks {
		types[i] = b.Type
	}
	return strings.Join(types, ",")
}

func TestBuildQuestionBlocks_SingleQuestionButtons(t *testing.T) {
	t.Parallel()
	longLabel := strings.Repeat("长", 100)
	card := platform.QuestionCard{
		ToolUseID: "toolu_1", ChatType: "group", AgentID: "reviewer",
		Items: []platform.QuestionItem{{
			Header: "Pick <!channel>", Question: "Which one & why?",
			Options: []platform.QuestionOption{
				{Label: "A <b>", Description: "first > second"},
				{Label: "B"},
				{Label: longLabel},
			},
		}},
	}
	blocks, text := renderBlocks(t, card)
	if got := blockTypes(blocks); got != "section,context,actions,context" {
		t.Fatalf("block types = %s", got)
	}
	head := blocks[0].Text.Text
	if strings.Contains(head, "<!channel>") || !strings.Contains(head, "&lt;!channel&gt;") ||
		!strings.Contains(head, "Which one &amp; why?") {
		t.Errorf("header/question not escaped: %q", head)
	}
	if desc := blocks[1].Elements[0].text(); desc != "• *A &lt;b&gt;* — first &gt; second" {
		t.Errorf("descriptions = %q", desc)
	}
	buttons := blocks[2].Elements
	if len(buttons) != 3 {
		t.Fatalf("%d buttons, want 3", len(buttons))
	}
	for i, btn := range buttons {
		opt := card.Items[0].Options[i]
		if want := askActionIDPrefix + string(rune('0'+i)); btn.ActionID != want {
			t.Errorf("button %d action_id = %q, want %q", i, btn.ActionID, want)
		}
		if n := utf8.RuneCountInString(btn.text()); n > slackButtonTextMaxRunes {
			t.Errorf("button %d text has %d runes", i, n)
		}
		var got platform.AskAnswerPayload
		if err := json.Unmarshal([]byte(btn.Value), &got); err != nil {
			t.Fatalf("button %d value %q: %v", i, btn.Value, err)
		}
		if want := platform.NewAskAnswerPayload(card, card.Items[0], opt); got != want {
			t.Errorf("button %d value = %+v, want %+v", i, got, want)
		}
	}
	if !strings.Contains(buttons[0].Value, `"label":"A <b>"`) {
		t.Errorf("value is HTML-escaped: %s", buttons[0].Value)
	}
	if !strings.Contains(blocks[3].Elements[0].text(), "直接回复文字") {
		t.Errorf("no text-reply hint: %+v", blocks[3])
	}
	if want := "Claude 想请你确认：\n" + platform.RenderAskQuestionPlain(card.Items); text != want {
		t.Errorf("notification text = %q, want %q", text, want)
	}
}

func TestBuildQuestionBlocks_NoDescriptionsNoContextList(t *testing.T) {
	t.Parallel()
	blocks, _ := renderBlocks(t, platform.QuestionCard{Items: []platform.QuestionItem{{
		Question: "Q", MultiSelect: true,
		Options: []platform.QuestionOption{{Label: "A"}, {Label: "B"}},
	}}})
	if got := blockTypes(blocks); got != "section,actions,context" {
		t.Fatalf("block types = %s", got)
	}
	if !strings.Contains(blocks[2].Elements[0].text(), "可多选") {
		t.Errorf("multi-select hint missing: %+v", blocks[2])
	}
}

func TestBuildQuestionBlocks_MultiQuestionIsReadOnly(t *testing.T) {
	t.Parallel()
	items := []platform.QuestionItem{
		{Header: "H1", Question: "Q1 <x>", Options: []platform.QuestionOption{{Label: "A"}}},
		{Question: "Q2", Options: []platform.QuestionOption{{Label: "B", Description: "d"}}},
	}
	blocks, _ := renderBlocks(t, platform.QuestionCard{Items: items})
	if got := blockTypes(blocks); got != "section" {
		t.Fatalf("block types = %s, want one read-only section", got)
	}
	body := blocks[0].Text.Text
	for _, want := range []string{"一次回复全部", "【H1】Q1 &lt;x&gt;", "问题 2：Q2", "1. B — d"} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q lacks %q", body, want)
		}
	}
}

func TestBuildQuestionBlocks_LongDescriptionsAreClipped(t *testing.T) {
	t.Parallel()
	opts := make([]platform.QuestionOption, 3)
	for i := range opts {
		opts[i] = platform.QuestionOption{Label: string(rune('A' + i)), Description: strings.Repeat("述", 2000)}
	}
	blocks, _ := renderBlocks(t, platform.QuestionCard{Items: []platform.QuestionItem{{Question: "Q", Options: opts}}})
	if got := blockTypes(blocks); got != "section,context,actions,context" {
		t.Fatalf("block types = %s", got)
	}
	if n := utf8.RuneCountInString(blocks[1].Elements[0].text()); n > slackTextObjectMaxRunes {
		t.Errorf("description context has %d runes, over %d", n, slackTextObjectMaxRunes)
	}
	if n := len(blocks[2].Elements); n != 3 {
		t.Errorf("%d buttons, want 3", n)
	}
}

// TestBuildQuestionBlocks_OverLimitsErrors: a message past a Block Kit limit
// is rejected whole, so the card must error and let dispatch post text.
func TestBuildQuestionBlocks_OverLimitsErrors(t *testing.T) {
	t.Parallel()
	opts := func(n int, label string) []platform.QuestionOption {
		out := make([]platform.QuestionOption, n)
		for i := range out {
			out[i] = platform.QuestionOption{Label: label}
		}
		return out
	}
	cases := map[string]platform.QuestionCard{
		"no items":   {},
		"26 options": {Items: []platform.QuestionItem{{Question: "Q", Options: opts(26, "x")}}},
		"long value": {
			ToolUseID: strings.Repeat("t", 128), AgentID: strings.Repeat("a", 128),
			Items: []platform.QuestionItem{{Header: strings.Repeat("头", 128), Question: "Q", Options: opts(1, strings.Repeat("长", 512))}},
		},
		"long section": {Items: []platform.QuestionItem{{Question: strings.Repeat("q", 3000), Options: opts(1, "x")}}},
		"long multi": {Items: []platform.QuestionItem{
			{Question: strings.Repeat("q", 2000)}, {Question: strings.Repeat("q", 1000)},
		}},
	}
	for name, card := range cases {
		if _, _, err := buildQuestionBlocks(card); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// 25 options of a plain label is exactly at the limit.
	if _, _, err := buildQuestionBlocks(platform.QuestionCard{Items: []platform.QuestionItem{{Question: "Q", Options: opts(25, "x")}}}); err != nil {
		t.Errorf("25 options: %v", err)
	}
}

func TestSendQuestionCard_PostsBlocksAndReturnsRef(t *testing.T) {
	t.Parallel()
	s, api := newAPIAdapter(t)
	ref, err := s.SendQuestionCard(context.Background(), "C42", platform.QuestionCard{
		ToolUseID: "toolu_1",
		Items:     []platform.QuestionItem{{Question: "Q <x>", Options: []platform.QuestionOption{{Label: "A"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := platform.EncodeMessageRef("C42", "1700000000.000100"); ref != want {
		t.Errorf("ref = %q, want %q", ref, want)
	}
	calls := api.recorded()
	if len(calls) != 1 || calls[0].method != "chat.postMessage" {
		t.Fatalf("calls = %+v", calls)
	}
	form := calls[0].form
	if got := blockTypes(decodeBlocks(t, form.Get("blocks"))); got != "section,actions,context" {
		t.Errorf("posted blocks = %s", got)
	}
	if text := form.Get("text"); strings.Contains(text, "<x>") || !strings.Contains(text, "&lt;x&gt;") {
		t.Errorf("notification text not escaped: %q", text)
	}
}

// clickCallback is a block_actions callback for one click on a card in
// channel at ts 111.222.
func clickCallback(channel, user, actionID string, val any) slack.InteractionCallback {
	raw, _ := json.Marshal(val)
	cb := slack.InteractionCallback{Type: slack.InteractionTypeBlockActions}
	cb.Channel.ID = channel
	cb.User.ID = user
	cb.Container = slack.Container{Type: "message", MessageTs: "111.222", ChannelID: channel}
	cb.ActionCallback.BlockActions = []*slack.BlockAction{{ActionID: actionID, Value: string(raw)}}
	return cb
}

// clickAdapter returns an adapter whose handler records every dispatched
// message; call s.dispatch.Wait before reading them.
func clickAdapter(t *testing.T) (*Slack, *fakeWebAPI, func() []platform.IncomingMessage) {
	t.Helper()
	s, api := newAPIAdapter(t)
	var mu sync.Mutex
	var got []platform.IncomingMessage
	s.handler = func(_ context.Context, msg platform.IncomingMessage) {
		mu.Lock()
		got = append(got, msg)
		mu.Unlock()
	}
	return s, api, func() []platform.IncomingMessage {
		s.dispatch.Wait()
		mu.Lock()
		defer mu.Unlock()
		return append([]platform.IncomingMessage(nil), got...)
	}
}

func answer(chatType string) platform.AskAnswerPayload {
	return platform.AskAnswerPayload{
		Kind: platform.AskAnswerKind, ToolUseID: "toolu_1",
		Header: "Style", Label: "Errors", ChatType: chatType, AgentID: "reviewer",
	}
}

func TestHandleBlockActions_DispatchesAnswer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, channel, chatType, want string
	}{
		{"group value", "C1", "group", "group"},
		{"direct value", "D1", "direct", "direct"},
		{"tampered value in channel", "C1", "evil", "group"},
		{"tampered value in IM", "D1", "evil", "direct"},
		{"missing value in IM", "D1", "", "direct"},
		{"missing value in private channel", "G1", "", "group"},
		{"value wins over the IM prefix", "D1", "group", "group"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, _, msgs := clickAdapter(t)
			s.handleBlockActions(clickCallback(tc.channel, "U9", askActionIDPrefix+"0", answer(tc.chatType)))
			got := msgs()
			if len(got) != 1 {
				t.Fatalf("%d messages dispatched, want 1", len(got))
			}
			want := platform.IncomingMessage{
				Platform:  "slack",
				EventID:   "block_action:" + tc.channel + ":111.222:U9:toolu_1",
				MessageID: tc.channel + ":111.222",
				UserID:    "U9",
				ChatID:    tc.channel,
				ChatType:  tc.want,
				Text:      "Style: Errors.",
				AgentID:   "reviewer",
				MentionMe: true,
			}
			if g := got[0]; g.Platform != want.Platform || g.EventID != want.EventID ||
				g.MessageID != want.MessageID || g.UserID != want.UserID || g.ChatID != want.ChatID ||
				g.ChatType != want.ChatType || g.Text != want.Text || g.AgentID != want.AgentID ||
				g.MentionMe != want.MentionMe {
				t.Errorf("dispatched %+v\nwant       %+v", g, want)
			}
		})
	}
}

// TestHandleBlockActions_ReplaysShareEventID: a redelivered envelope and a
// second click by the same user on another option dedup as one answer; a
// different user's click does not.
func TestHandleBlockActions_ReplaysShareEventID(t *testing.T) {
	t.Parallel()
	s, _, msgs := clickAdapter(t)
	other := answer("group")
	other.Label = "Panics"
	s.handleBlockActions(clickCallback("C1", "U9", askActionIDPrefix+"0", answer("group")))
	s.handleBlockActions(clickCallback("C1", "U9", askActionIDPrefix+"0", answer("group")))
	s.handleBlockActions(clickCallback("C1", "U9", askActionIDPrefix+"1", other))
	s.handleBlockActions(clickCallback("C1", "U8", askActionIDPrefix+"0", answer("group")))
	got := msgs()
	if len(got) != 4 {
		t.Fatalf("%d messages, want 4", len(got))
	}
	ids := map[string]int{}
	for _, m := range got {
		ids[m.EventID]++
	}
	if ids["block_action:C1:111.222:U9:toolu_1"] != 3 || ids["block_action:C1:111.222:U8:toolu_1"] != 1 {
		t.Errorf("event IDs = %v", ids)
	}
}

func TestHandleBlockActions_IgnoresForeignClicks(t *testing.T) {
	t.Parallel()
	wrongKind := answer("group")
	wrongKind.Kind = "perm_answer"
	noLabel := answer("group")
	noLabel.Label = " "
	cases := map[string]slack.InteractionCallback{
		"foreign action_id": clickCallback("C1", "U9", "other_app_0", answer("group")),
		"wrong kind":        clickCallback("C1", "U9", askActionIDPrefix+"0", wrongKind),
		"empty label":       clickCallback("C1", "U9", askActionIDPrefix+"0", noLabel),
		"undecodable value": clickCallback("C1", "U9", askActionIDPrefix+"0", "not an object"),
		"no channel":        clickCallback("", "U9", askActionIDPrefix+"0", answer("group")),
		"no actions":        {Type: slack.InteractionTypeBlockActions},
	}
	for name, cb := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, api, msgs := clickAdapter(t)
			s.handleBlockActions(cb)
			if got := msgs(); len(got) != 0 {
				t.Errorf("dispatched %+v", got)
			}
			if calls := api.recorded(); len(calls) != 0 {
				t.Errorf("Web API calls %+v", calls)
			}
		})
	}
}

// TestHandleBlockActions_FallsBackToContainerChannel: the channel object can
// be absent on some surfaces; the container still names the card's channel.
func TestHandleBlockActions_FallsBackToContainerChannel(t *testing.T) {
	t.Parallel()
	s, _, msgs := clickAdapter(t)
	cb := clickCallback("D7", "U9", askActionIDPrefix+"0", answer(""))
	cb.Channel.ID = ""
	s.handleBlockActions(cb)
	got := msgs()
	if len(got) != 1 || got[0].ChatID != "D7" || got[0].ChatType != "direct" {
		t.Fatalf("dispatched %+v", got)
	}
}

func TestHandleBlockActions_ReplacesCardBlocks(t *testing.T) {
	t.Parallel()
	s, api, msgs := clickAdapter(t)
	val := answer("group")
	val.Label = "<!here>"
	s.handleBlockActions(clickCallback("C1", "U9", askActionIDPrefix+"0", val))
	msgs()
	calls := api.recorded()
	if len(calls) != 1 || calls[0].method != "chat.update" {
		t.Fatalf("calls = %+v", calls)
	}
	form := calls[0].form
	if form.Get("channel") != "C1" || form.Get("ts") != "111.222" {
		t.Errorf("updated %s/%s, want C1/111.222", form.Get("channel"), form.Get("ts"))
	}
	blocks := decodeBlocks(t, form.Get("blocks"))
	if got := blockTypes(blocks); got != "section" {
		t.Fatalf("replacement blocks = %s, want one section with no buttons", got)
	}
	if text := blocks[0].Text.Text; text != "✅ 已回答：Style: &lt;!here&gt;." {
		t.Errorf("replacement text = %q", text)
	}
}

// TestHandleBlockActions_DroppedAnswerKeepsButtons: when the dispatch
// semaphore is full the answer is dropped, so the card must stay clickable.
func TestHandleBlockActions_DroppedAnswerKeepsButtons(t *testing.T) {
	t.Parallel()
	s, api, msgs := clickAdapter(t)
	s.dispatch.Cap = 1
	if !s.dispatch.TryAcquire() {
		t.Fatal("TryAcquire")
	}
	s.handleBlockActions(clickCallback("C1", "U9", askActionIDPrefix+"0", answer("group")))
	s.dispatch.Release()
	if got := msgs(); len(got) != 0 {
		t.Errorf("dispatched %+v", got)
	}
	if calls := api.recorded(); len(calls) != 0 {
		t.Errorf("card edited although the answer was dropped: %+v", calls)
	}
}

// TestSocketMode_BlockActionsAckedAndAnswered drives a button click through
// the real socket mode client: the envelope is acked, the answer dispatched
// and the card's buttons replaced.
func TestSocketMode_BlockActionsAckedAndAnswered(t *testing.T) {
	t.Parallel()
	f := newFakeSlack(t)
	api := &fakeWebAPI{}
	f.mux.Handle("/chat.update", api.handler())
	got := make(chan platform.IncomingMessage, 1)
	f.handler = func(_ context.Context, msg platform.IncomingMessage) { got <- msg }
	s := f.adapter()
	t.Cleanup(func() { _ = s.Stop() })
	f.start(t, s)
	f.awaitOpen(t, "initial")
	f.reply(t, map[string]any{"ok": true, "url": f.wsURL()})
	conn := f.awaitConn(t)

	value, _ := json.Marshal(answer("group"))
	if err := conn.WriteJSON(map[string]any{
		"type": "interactive", "envelope_id": "env-1",
		"payload": map[string]any{
			"type":      "block_actions",
			"user":      map[string]any{"id": "U9"},
			"channel":   map[string]any{"id": "C1"},
			"container": map[string]any{"type": "message", "message_ts": "111.222", "channel_id": "C1"},
			"actions": []map[string]any{{
				"type": "button", "action_id": askActionIDPrefix + "0", "block_id": "b", "value": string(value),
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	for acked := false; !acked; {
		select {
		case frame := <-f.inbound:
			var res struct {
				EnvelopeID string `json:"envelope_id"`
			}
			acked = json.Unmarshal(frame, &res) == nil && res.EnvelopeID == "env-1"
		case <-time.After(connStateTestTimeout):
			t.Fatal("interactive envelope never acked")
		}
	}
	select {
	case msg := <-got:
		if msg.Text != "Style: Errors." || msg.ChatID != "C1" || msg.AgentID != "reviewer" || !msg.MentionMe {
			t.Errorf("dispatched %+v", msg)
		}
	case <-time.After(connStateTestTimeout):
		t.Fatal("answer never dispatched")
	}
	testhelper.Eventually(t, func() bool { return len(api.recorded()) == 1 },
		connStateTestTimeout, "chat.update after the click")
}
