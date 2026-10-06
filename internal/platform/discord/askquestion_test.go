package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/naozhi/naozhi/internal/platform"
)

var _ platform.QuestionCardSender = (*Discord)(nil)

// restCall is one REST request the recorder saw.
type restCall struct {
	method, path string
	body         []byte
}

// restRecorder answers message posts with message "M1", interaction
// callbacks with 204, and records every call. A non-zero status fails all.
type restRecorder struct {
	mu     sync.Mutex
	calls  []restCall
	status int
}

func (r *restRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	r.mu.Lock()
	r.calls = append(r.calls, restCall{method: req.Method, path: req.URL.Path, body: body})
	status := r.status
	r.mu.Unlock()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: req,
		Body: io.NopCloser(strings.NewReader(`{"id":"M1","channel_id":"C1"}`))}
	switch {
	case status != 0:
		resp.StatusCode = status
		resp.Body = io.NopCloser(strings.NewReader(`{"message":"rejected","code":50035}`))
	case strings.Contains(req.URL.Path, "/interactions/"):
		resp.StatusCode = http.StatusNoContent
		resp.Body = http.NoBody
	}
	resp.Header.Set("Content-Type", "application/json")
	return resp, nil
}

func (r *restRecorder) recorded() []restCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]restCall(nil), r.calls...)
}

// questionAdapter is a Discord with a REST-only session, botID "bot-1" and a
// handler that records what it receives.
func questionAdapter(t *testing.T) (*Discord, *restRecorder, func() []platform.IncomingMessage) {
	t.Helper()
	rec := &restRecorder{}
	d := New(Config{BotToken: "test-token"})
	d.restTransport = rec
	sess, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	d.configureSession(sess)
	d.session = sess
	d.setBotID("bot-1")
	var mu sync.Mutex
	var got []platform.IncomingMessage
	d.handler = func(_ context.Context, m platform.IncomingMessage) {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	}
	return d, rec, func() []platform.IncomingMessage {
		d.dispatch.Wait()
		mu.Lock()
		defer mu.Unlock()
		return append([]platform.IncomingMessage(nil), got...)
	}
}

func oneQuestion(n int) platform.QuestionCard {
	item := platform.QuestionItem{Header: "Style", Question: "Which style?"}
	for i := range n {
		item.Options = append(item.Options, platform.QuestionOption{
			Label: "opt " + string(rune('A'+i)), Description: "desc " + string(rune('A'+i)),
		})
	}
	return platform.QuestionCard{ToolUseID: "tu-1", AgentID: "reviewer", Items: []platform.QuestionItem{item}}
}

// buttons flattens the action rows of a built message.
func buttons(t *testing.T, ms *discordgo.MessageSend) []discordgo.Button {
	t.Helper()
	var out []discordgo.Button
	for _, c := range ms.Components {
		row, ok := c.(discordgo.ActionsRow)
		if !ok {
			t.Fatalf("component %T, want ActionsRow", c)
		}
		if len(row.Components) > discordButtonsPerRow {
			t.Fatalf("row has %d buttons, over %d", len(row.Components), discordButtonsPerRow)
		}
		for _, e := range row.Components {
			out = append(out, e.(discordgo.Button))
		}
	}
	return out
}

// deliveredCard round-trips ms through JSON into the message Discord would
// attach to an interaction, so recovery runs on decoded component types.
func deliveredCard(t *testing.T, ms *discordgo.MessageSend, authorID string) *discordgo.Message {
	t.Helper()
	raw, err := json.Marshal(ms)
	if err != nil {
		t.Fatal(err)
	}
	var m discordgo.Message
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m.ID, m.ChannelID, m.Author = "M1", "C1", &discordgo.User{ID: authorID}
	return &m
}

// click is a button interaction on card, by user U9 in guild (or a DM).
func click(card *discordgo.Message, customID string, guild bool) *discordgo.InteractionCreate {
	i := &discordgo.Interaction{
		ID: "I1", Type: discordgo.InteractionMessageComponent, Token: "tok",
		ChannelID: "C1", Message: card,
		Data: discordgo.MessageComponentInteractionData{CustomID: customID, ComponentType: discordgo.ButtonComponent},
	}
	if guild {
		i.GuildID = "G1"
		i.Member = &discordgo.Member{User: &discordgo.User{ID: "U9"}}
	} else {
		i.User = &discordgo.User{ID: "U9"}
	}
	return &discordgo.InteractionCreate{Interaction: i}
}

func TestBuildQuestionMessage_SingleQuestionButtons(t *testing.T) {
	t.Parallel()
	ms, err := buildQuestionMessage(oneQuestion(7))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms.Components) != 2 {
		t.Fatalf("%d action rows, want 2 (5 + 2)", len(ms.Components))
	}
	btns := buttons(t, ms)
	if len(btns) != 7 {
		t.Fatalf("%d buttons, want 7", len(btns))
	}
	for i, b := range btns {
		if want := "naq:" + string(rune('0'+i)) + ":reviewer"; b.CustomID != want {
			t.Errorf("button %d custom_id = %q, want %q", i, b.CustomID, want)
		}
		if want := "opt " + string(rune('A'+i)); b.Label != want {
			t.Errorf("button %d label = %q, want %q", i, b.Label, want)
		}
	}
	if len(ms.Embeds) != 1 {
		t.Fatalf("%d embeds, want 1", len(ms.Embeds))
	}
	e := ms.Embeds[0]
	if e.Title != "Style" {
		t.Errorf("title = %q, want the raw header", e.Title)
	}
	if !strings.HasPrefix(e.Description, "Which style?\n**opt A** — desc A") {
		t.Errorf("description = %q", e.Description)
	}
	if e.Footer == nil || e.Footer.Text != "也可以直接回复文字作答。" {
		t.Errorf("footer = %+v", e.Footer)
	}
	if ms.AllowedMentions == nil || ms.AllowedMentions.Parse == nil || len(ms.AllowedMentions.Parse) != 0 {
		t.Errorf("allowed mentions = %+v, want none", ms.AllowedMentions)
	}

	multi := oneQuestion(2)
	multi.Items[0].MultiSelect = true
	ms, err = buildQuestionMessage(multi)
	if err != nil {
		t.Fatal(err)
	}
	if got := ms.Embeds[0].Footer.Text; !strings.Contains(got, "可多选") {
		t.Errorf("multi-select footer = %q", got)
	}
}

func TestBuildQuestionMessage_MultiQuestionIsReadOnly(t *testing.T) {
	t.Parallel()
	card := oneQuestion(2)
	card.Items = append(card.Items, platform.QuestionItem{Question: "Second?",
		Options: []platform.QuestionOption{{Label: "yes"}}})
	ms, err := buildQuestionMessage(card)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms.Components) != 0 {
		t.Errorf("multi-question card has %d action rows, want none", len(ms.Components))
	}
	desc := ms.Embeds[0].Description
	for _, want := range []string{"Which style?", "Second?", "opt A"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description %q lacks %q", desc, want)
		}
	}
}

func TestBuildQuestionMessage_EscapesMarkdown(t *testing.T) {
	t.Parallel()
	card := oneQuestion(1)
	card.Items[0].Question = "**bold** <@123> [x](http://e)"
	card.Items[0].Options[0].Description = "_it_"
	ms, err := buildQuestionMessage(card)
	if err != nil {
		t.Fatal(err)
	}
	want := `\*\*bold\*\* \<\@123\> \[x\]\(http://e\)` + "\n**opt A** — \\_it\\_"
	if got := ms.Embeds[0].Description; got != want {
		t.Errorf("description = %q, want %q", got, want)
	}
}

// TestBuildQuestionMessage_LongDescriptionsAreClipped: option descriptions
// only explain, so they are clipped to the embed limit; the buttons stay.
func TestBuildQuestionMessage_LongDescriptionsAreClipped(t *testing.T) {
	t.Parallel()
	card := oneQuestion(5)
	for i := range card.Items[0].Options {
		card.Items[0].Options[i].Description = strings.Repeat("长", 2000)
	}
	ms, err := buildQuestionMessage(card)
	if err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCountInString(ms.Embeds[0].Description); n > discordEmbedDescMaxRunes {
		t.Errorf("description has %d runes, over %d", n, discordEmbedDescMaxRunes)
	}
	if n := len(buttons(t, ms)); n != 5 {
		t.Errorf("%d buttons, want 5", n)
	}
}

// TestBuildQuestionMessage_OverLimitsErrors: anything Discord would reject,
// or a label a button would shorten, errors so dispatch falls back to text.
func TestBuildQuestionMessage_OverLimitsErrors(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*platform.QuestionCard){
		"no items":   func(c *platform.QuestionCard) { c.Items = nil },
		"26 options": func(c *platform.QuestionCard) { *c = oneQuestion(26) },
		"81-rune label": func(c *platform.QuestionCard) {
			c.Items[0].Options[1].Label = strings.Repeat("长", 81)
		},
		"empty label": func(c *platform.QuestionCard) { c.Items[0].Options[0].Label = "" },
		"custom_id over 100 bytes": func(c *platform.QuestionCard) {
			c.AgentID = strings.Repeat("a", 95)
		},
		"question over the embed limit": func(c *platform.QuestionCard) {
			c.Items[0].Question = strings.Repeat("q", discordEmbedDescMaxRunes+1)
		},
		"multi-question body over the embed limit": func(c *platform.QuestionCard) {
			c.Items = append(c.Items, platform.QuestionItem{Question: strings.Repeat("q", discordEmbedDescMaxRunes)})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			card := oneQuestion(3)
			mutate(&card)
			if ms, err := buildQuestionMessage(card); err == nil {
				t.Fatalf("no error; built %+v", ms)
			}
		})
	}
	// The boundaries themselves still build.
	card := oneQuestion(25)
	card.Items[0].Options[0].Label = strings.Repeat("长", discordButtonLabelMaxRunes)
	card.AgentID = strings.Repeat("a", discordCustomIDMaxBytes-len("naq:24:"))
	card.Items[0].Header = strings.Repeat("h", 300) // over the 256-rune title limit
	ms, err := buildQuestionMessage(card)
	if err != nil {
		t.Fatalf("card at the limits: %v", err)
	}
	if n := utf8.RuneCountInString(ms.Embeds[0].Title); n != platform.AskHeaderMaxRunes {
		t.Errorf("title has %d runes, want the header capped to %d", n, platform.AskHeaderMaxRunes)
	}
}

func TestSendQuestionCard_PostsComponents(t *testing.T) {
	t.Parallel()
	d, rec, _ := questionAdapter(t)
	ref, err := d.SendQuestionCard(context.Background(), "C1", oneQuestion(2))
	if err != nil {
		t.Fatal(err)
	}
	if ref != "C1:M1" {
		t.Errorf("ref = %q, want C1:M1", ref)
	}
	calls := rec.recorded()
	if len(calls) != 1 || calls[0].method != http.MethodPost || !strings.HasSuffix(calls[0].path, "/channels/C1/messages") {
		t.Fatalf("calls = %+v", calls)
	}
	var sent struct {
		Components []struct {
			Components []struct {
				CustomID string `json:"custom_id"`
				Label    string `json:"label"`
			} `json:"components"`
		} `json:"components"`
		AllowedMentions *struct {
			Parse []string `json:"parse"`
		} `json:"allowed_mentions"`
	}
	if err := json.Unmarshal(calls[0].body, &sent); err != nil {
		t.Fatalf("body %s: %v", calls[0].body, err)
	}
	if len(sent.Components) != 1 || len(sent.Components[0].Components) != 2 ||
		sent.Components[0].Components[1].CustomID != "naq:1:reviewer" {
		t.Errorf("components = %+v", sent.Components)
	}
	if sent.AllowedMentions == nil || sent.AllowedMentions.Parse == nil || len(sent.AllowedMentions.Parse) != 0 {
		t.Errorf("allowed_mentions = %s, want parse: []", calls[0].body)
	}

	rec.mu.Lock()
	rec.status = http.StatusBadRequest
	rec.mu.Unlock()
	if _, err := d.SendQuestionCard(context.Background(), "C1", oneQuestion(2)); err == nil {
		t.Error("rejected post returned no error; dispatch would skip the text fallback")
	}
}

// callbackBody decodes an interaction response the recorder saw.
type callbackBody struct {
	Type discordgo.InteractionResponseType `json:"type"`
	Data struct {
		Content         string            `json:"content"`
		Components      []json.RawMessage `json:"components"`
		Embeds          []json.RawMessage `json:"embeds"`
		AllowedMentions *struct {
			Parse []string `json:"parse"`
		} `json:"allowed_mentions"`
	} `json:"data"`
}

func assertCardReplaced(t *testing.T, body []byte, wantContent string) {
	t.Helper()
	var cb callbackBody
	if err := json.Unmarshal(body, &cb); err != nil {
		t.Fatalf("callback %s: %v", body, err)
	}
	if cb.Type != discordgo.InteractionResponseUpdateMessage {
		t.Errorf("response type = %d, want UpdateMessage (7)", cb.Type)
	}
	if cb.Data.Content != wantContent {
		t.Errorf("content = %q, want %q", cb.Data.Content, wantContent)
	}
	if cb.Data.Components == nil || len(cb.Data.Components) != 0 || cb.Data.Embeds == nil || len(cb.Data.Embeds) != 0 {
		t.Errorf("components/embeds not cleared: %s", body)
	}
	if cb.Data.AllowedMentions == nil || cb.Data.AllowedMentions.Parse == nil || len(cb.Data.AllowedMentions.Parse) != 0 {
		t.Errorf("allowed_mentions = %s, want parse: []", body)
	}
}

func TestOnInteractionCreate_DispatchesAnswer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		header       string
		guild        bool
		wantChatType string
		wantText     string
	}{
		{"guild click", "Style", true, "group", "Style: opt B."},
		{"dm click", "Style", false, "direct", "Style: opt B."},
		{"no header", "", true, "group", "opt B."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, rec, msgs := questionAdapter(t)
			card := oneQuestion(3)
			card.Items[0].Header = tc.header
			ms, err := buildQuestionMessage(card)
			if err != nil {
				t.Fatal(err)
			}
			d.onInteractionCreate(nil, click(deliveredCard(t, ms, "bot-1"), "naq:1:reviewer", tc.guild))
			got := msgs()
			if len(got) != 1 {
				t.Fatalf("dispatched %d messages, want 1", len(got))
			}
			want := platform.IncomingMessage{
				Platform: "discord", EventID: "component:C1:M1:U9", MessageID: "C1:M1",
				UserID: "U9", ChatID: "C1", ChatType: tc.wantChatType, Text: tc.wantText,
				AgentID: "reviewer", MentionMe: true,
			}
			if got[0].Platform != want.Platform || got[0].EventID != want.EventID ||
				got[0].MessageID != want.MessageID || got[0].UserID != want.UserID ||
				got[0].ChatID != want.ChatID || got[0].ChatType != want.ChatType ||
				got[0].Text != want.Text || got[0].AgentID != want.AgentID || !got[0].MentionMe {
				t.Errorf("message = %+v\nwant      %+v", got[0], want)
			}
			calls := rec.recorded()
			if len(calls) != 1 || !strings.HasSuffix(calls[0].path, "/interactions/I1/tok/callback") {
				t.Fatalf("calls = %+v", calls)
			}
			assertCardReplaced(t, calls[0].body, "✅ 已回答："+escapeMarkdown(tc.wantText))
		})
	}
}

// TestOnInteractionCreate_IgnoresForeignClicks: only a naq: button on the
// bot's own card is answered; nothing else is dispatched or responded to.
func TestOnInteractionCreate_IgnoresForeignClicks(t *testing.T) {
	t.Parallel()
	ms, err := buildQuestionMessage(oneQuestion(2))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*testing.T, *Discord) *discordgo.InteractionCreate{
		"application command": func(t *testing.T, _ *Discord) *discordgo.InteractionCreate {
			ic := click(deliveredCard(t, ms, "bot-1"), "naq:0:reviewer", true)
			ic.Type = discordgo.InteractionApplicationCommand
			return ic
		},
		"foreign custom_id on the card": func(t *testing.T, _ *Discord) *discordgo.InteractionCreate {
			card := deliveredCard(t, ms, "bot-1")
			card.Components[0].(*discordgo.ActionsRow).Components[0].(*discordgo.Button).CustomID = "other:0"
			return click(card, "other:0", true)
		},
		"card by another author": func(t *testing.T, _ *Discord) *discordgo.InteractionCreate {
			return click(deliveredCard(t, ms, "someone-else"), "naq:0:reviewer", true)
		},
		"bot identity unknown": func(t *testing.T, d *Discord) *discordgo.InteractionCreate {
			d.botID.Store(nil)
			return click(deliveredCard(t, ms, "bot-1"), "naq:0:reviewer", true)
		},
		"no message": func(t *testing.T, _ *Discord) *discordgo.InteractionCreate {
			return click(nil, "naq:0:reviewer", true)
		},
		"button not on the card": func(t *testing.T, _ *Discord) *discordgo.InteractionCreate {
			return click(deliveredCard(t, ms, "bot-1"), "naq:9:reviewer", true)
		},
		"no user": func(t *testing.T, _ *Discord) *discordgo.InteractionCreate {
			ic := click(deliveredCard(t, ms, "bot-1"), "naq:0:reviewer", true)
			ic.Member = nil
			return ic
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, rec, msgs := questionAdapter(t)
			d.onInteractionCreate(nil, build(t, d))
			if got := msgs(); len(got) != 0 {
				t.Errorf("dispatched %+v", got)
			}
			for _, c := range rec.recorded() {
				if strings.Contains(c.path, "/interactions/") {
					t.Errorf("responded to an ignored click: %+v", c)
				}
			}
		})
	}
}

// TestOnInteractionCreate_DroppedAnswerKeepsButtons: with the dispatch
// semaphore full there is no response, so the card stays clickable.
func TestOnInteractionCreate_DroppedAnswerKeepsButtons(t *testing.T) {
	t.Parallel()
	d, rec, msgs := questionAdapter(t)
	d.dispatch.Cap = 1
	if !d.dispatch.TryAcquire() {
		t.Fatal("TryAcquire")
	}
	ms, err := buildQuestionMessage(oneQuestion(2))
	if err != nil {
		t.Fatal(err)
	}
	d.onInteractionCreate(nil, click(deliveredCard(t, ms, "bot-1"), "naq:0:reviewer", true))
	d.dispatch.Release()
	if got := msgs(); len(got) != 0 {
		t.Errorf("dispatched %+v", got)
	}
	if calls := rec.recorded(); len(calls) != 0 {
		t.Errorf("card edited although the answer was dropped: %+v", calls)
	}
}

// TestOnInteractionCreate_FailedEditStillAnswers: the card edit is best
// effort; a rejected response must not lose the answer.
func TestOnInteractionCreate_FailedEditStillAnswers(t *testing.T) {
	t.Parallel()
	d, rec, msgs := questionAdapter(t)
	rec.status = http.StatusBadRequest
	ms, err := buildQuestionMessage(oneQuestion(2))
	if err != nil {
		t.Fatal(err)
	}
	d.onInteractionCreate(nil, click(deliveredCard(t, ms, "bot-1"), "naq:0:reviewer", true))
	if got := msgs(); len(got) != 1 || got[0].Text != "Style: opt A." {
		t.Errorf("dispatched %+v, want the answer", got)
	}
}

// TestGateway_InteractionCreateAnswered drives a click through a real
// discordgo session: the gateway event reaches the handler and the card is
// replaced through the interaction callback.
func TestGateway_InteractionCreateAnswered(t *testing.T) {
	t.Parallel()
	g := newFakeGateway(t, 0)
	d := newGatewayAdapter(t, g)
	got := make(chan platform.IncomingMessage, 1)
	g.hello <- struct{}{}
	if err := d.Start(func(_ context.Context, m platform.IncomingMessage) { got <- m }); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })
	conn := <-g.conns

	ms, err := buildQuestionMessage(oneQuestion(2))
	if err != nil {
		t.Fatal(err)
	}
	// discordgo.Message does not marshal its components; send the card as
	// posted plus the fields Discord adds.
	raw, err := json.Marshal(ms)
	if err != nil {
		t.Fatal(err)
	}
	var card map[string]any
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatal(err)
	}
	card["id"], card["channel_id"], card["author"] = "M1", "C1", map[string]any{"id": "bot-1"}
	if err := conn.WriteJSON(map[string]any{"op": 0, "s": 3, "t": "INTERACTION_CREATE", "d": map[string]any{
		"id": "I1", "type": 3, "token": "tok", "guild_id": "G1", "channel_id": "C1",
		"data":    map[string]any{"custom_id": "naq:1:reviewer", "component_type": 2},
		"member":  map[string]any{"user": map[string]any{"id": "U9"}},
		"message": card,
	}}); err != nil {
		t.Fatal(err)
	}

	select {
	case m := <-got:
		if m.Text != "Style: opt B." || m.AgentID != "reviewer" || m.ChatType != "group" {
			t.Errorf("message = %+v", m)
		}
	case <-time.After(connStateTestTimeout):
		t.Fatal("click never reached the handler")
	}
	select {
	case body := <-g.callbacks:
		assertCardReplaced(t, bytes.TrimSpace(body), "✅ 已回答："+escapeMarkdown("Style: opt B."))
	case <-time.After(connStateTestTimeout):
		t.Fatal("card was never replaced")
	}
}
