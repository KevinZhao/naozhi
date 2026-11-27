package feishu

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// Verify Feishu implements RunnablePlatform at compile time.
var (
	_ platform.RunnablePlatform   = (*Feishu)(nil)
	_ platform.Reactor            = (*Feishu)(nil)
	_ platform.QuestionCardSender = (*Feishu)(nil)
	_ platform.ConnStateReporter  = (*Feishu)(nil)
)

// --- parseSDKEvent tests ---

func strPtr(s string) *string { return &s }

func TestParseSDKEvent_TextMessage(t *testing.T) {
	t.Parallel()
	event := &larkim.P2MessageReceiveV1{
		EventV2Base: &larkevent.EventV2Base{
			Header: &larkevent.EventHeader{EventID: "ev_123"},
		},
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: strPtr("ou_user1")},
			},
			Message: &larkim.EventMessage{
				MessageType: strPtr("text"),
				ChatId:      strPtr("oc_chat1"),
				ChatType:    strPtr("group"),
				Content:     strPtr(`{"text":"hello world"}`),
				Mentions:    nil,
			},
		},
	}

	pe, ok := (&Feishu{}).parseSDKEvent(event)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if pe.MediaKey != "" {
		t.Errorf("MediaKey = %q, want empty for text message", pe.MediaKey)
	}
	if pe.Msg.Platform != "feishu" {
		t.Errorf("Platform = %q, want feishu", pe.Msg.Platform)
	}
	if pe.Msg.EventID != "ev_123" {
		t.Errorf("EventID = %q, want ev_123", pe.Msg.EventID)
	}
	if pe.Msg.UserID != "ou_user1" {
		t.Errorf("UserID = %q, want ou_user1", pe.Msg.UserID)
	}
	if pe.Msg.ChatID != "oc_chat1" {
		t.Errorf("ChatID = %q, want oc_chat1", pe.Msg.ChatID)
	}
	if pe.Msg.ChatType != "group" {
		t.Errorf("ChatType = %q, want group", pe.Msg.ChatType)
	}
	if pe.Msg.Text != "hello world" {
		t.Errorf("Text = %q, want 'hello world'", pe.Msg.Text)
	}
	if pe.Msg.MentionMe {
		t.Error("MentionMe should be false")
	}
}

func TestParseSDKEvent_WithMentions(t *testing.T) {
	t.Parallel()
	event := &larkim.P2MessageReceiveV1{
		EventV2Base: &larkevent.EventV2Base{
			Header: &larkevent.EventHeader{EventID: "ev_789"},
		},
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: strPtr("ou_user3")},
			},
			Message: &larkim.EventMessage{
				MessageType: strPtr("text"),
				ChatId:      strPtr("oc_chat3"),
				ChatType:    strPtr("group"),
				Content:     strPtr(`{"text":"@_user_1 do something"}`),
				Mentions: []*larkim.MentionEvent{
					{Key: strPtr("@_user_1"), Name: strPtr("Bot")},
				},
			},
		},
	}

	pe, ok := (&Feishu{}).parseSDKEvent(event)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if pe.Msg.Text != "do something" {
		t.Errorf("Text = %q, want 'do something'", pe.Msg.Text)
	}
	if !pe.Msg.MentionMe {
		t.Error("MentionMe should be true")
	}
}

// TestParseSDKEvent_Rejected: events with nothing routable are dropped.
func TestParseSDKEvent_Rejected(t *testing.T) {
	t.Parallel()
	sender := &larkim.EventSender{SenderId: &larkim.UserId{OpenId: strPtr("ou_user1")}}
	msgEvent := func(m *larkim.EventMessage) *larkim.P2MessageReceiveV1 {
		return &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{Sender: sender, Message: m}}
	}
	tests := []struct {
		name  string
		event *larkim.P2MessageReceiveV1
	}{
		{"nil event", nil},
		{"nil message", &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{}}},
		{"image with empty image_key", msgEvent(&larkim.EventMessage{
			MessageType: strPtr("image"), ChatId: strPtr("oc_chat1"), Content: strPtr(`{"image_key":""}`),
		})},
		{"audio with empty file_key", msgEvent(&larkim.EventMessage{
			MessageType: strPtr("audio"), ChatId: strPtr("oc_chat1"), Content: strPtr(`{"file_key":""}`),
		})},
		{"empty text", msgEvent(&larkim.EventMessage{
			MessageType: strPtr("text"), ChatId: strPtr("oc_chat1"), Content: strPtr(`{"text":"  "}`),
		})},
		{"mention-only text", msgEvent(&larkim.EventMessage{
			MessageType: strPtr("text"), ChatId: strPtr("oc_chat1"), ChatType: strPtr("group"),
			Content:  strPtr(`{"text":"@_user_1"}`),
			Mentions: []*larkim.MentionEvent{{Key: strPtr("@_user_1"), Name: strPtr("Bot")}},
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := (&Feishu{}).parseSDKEvent(tt.event); ok {
				t.Error("expected ok=false")
			}
		})
	}
}

func TestParseSDKEvent_UnsupportedType(t *testing.T) {
	t.Parallel()
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Message: &larkim.EventMessage{
				MessageType: strPtr("sticker"),
				ChatId:      strPtr("oc_chat1"),
				Content:     strPtr(`{"file_key":"file_v3_abc"}`),
			},
		},
	}
	// Parsed rather than dropped, so routeParsed can tell the user (#3626).
	pe, ok := (&Feishu{}).parseSDKEvent(event)
	if !ok || pe.Unsupported != "sticker" || pe.MediaType != "" {
		t.Errorf("parseSDKEvent(sticker) = %+v, %v; want Unsupported=sticker", pe, ok)
	}
}

// TestParseSDKEvent_MediaMessage: image and voice messages carry their
// resource key and message ID, with no text; a p2p chat is "direct".
func TestParseSDKEvent_MediaMessage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ msgType, content, key string }{
		{"image", `{"image_key":"img_v3_xxx"}`, "img_v3_xxx"},
		{"audio", `{"file_key":"file_v3_audio_xxx"}`, "file_v3_audio_xxx"},
	} {
		pe, ok := (&Feishu{}).parseSDKEvent(sdkMediaEvent("ev_m", tc.msgType, tc.content))
		if !ok {
			t.Fatalf("%s: parseSDKEvent rejected it", tc.msgType)
		}
		if pe.MediaType != tc.msgType || pe.MediaKey != tc.key || pe.MessageID != "om_ev_m" || pe.Msg.Text != "" {
			t.Errorf("%s: parsed %+v, want type %s key %s, no text", tc.msgType, pe, tc.msgType, tc.key)
		}
		if pe.Msg.ChatType != "direct" {
			t.Errorf("%s: ChatType = %q, want direct for p2p", tc.msgType, pe.Msg.ChatType)
		}
	}
}

// --- MentionMe precision (isBotMentioned) + fetchBotInfo tests ---

// TestIsBotMentioned: with the bot's open_id known only a mention of it
// counts (entries with an empty open_id are skipped); with it unknown
// (fetchBotInfo failed or has not run) any mention counts.
func TestIsBotMentioned(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, botID string
		openIDs     []string
		want        bool
	}{
		{"exact match", "ou_bot_self", []string{"ou_alice", "ou_bot_self", "ou_bob"}, true},
		{"no match", "ou_bot_self", []string{"ou_alice", "ou_bob"}, false},
		{"match after an empty id", "ou_bot_self", []string{"", "ou_bot_self"}, true},
		{"only empty ids", "ou_bot_self", []string{"", ""}, false},
		{"unknown bot id, any mention", "", []string{"", "", ""}, true},
		{"unknown bot id, no mentions", "", nil, false},
	}
	for _, tc := range cases {
		f := &Feishu{botOpenID: tc.botID}
		if got := f.isBotMentioned(len(tc.openIDs), func(i int) string { return tc.openIDs[i] }); got != tc.want {
			t.Errorf("%s: isBotMentioned = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestMaybeRefreshBotInfo_SelfHeals: when isBotMentioned is in the degraded
// "any @" fallback (open_id unknown), a group mention triggers a background
// re-fetch that populates botOpenID so subsequent mentions match strictly.
// Locks in R229-ARCH-19 (#1009).
func TestMaybeRefreshBotInfo_SelfHeals(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t_abc","expire":7200}`))
		case "/open-apis/bot/v3/info":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","bot":{"open_id":"ou_bot_self","app_name":"NaozhiBot"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := New(Config{AppID: "app", AppSecret: "secret"}, nil)
	defer func() { _ = f.Stop() }()
	f.baseURL = server.URL

	// open_id unknown → degraded path returns true for any mention AND kicks
	// the lazy re-fetch.
	if !f.isBotMentioned(1, func(int) string { return "ou_someone_else" }) {
		t.Fatal("degraded path should match any mention while open_id unknown")
	}

	// Wait for the background re-fetch to populate open_id.
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.botInfoMu.RLock()
		got := f.botOpenID
		f.botInfoMu.RUnlock()
		if got == "ou_bot_self" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("botOpenID was not self-healed within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Now strict matching applies: an @other-bot mention no longer wakes us.
	if f.isBotMentioned(1, func(int) string { return "ou_someone_else" }) {
		t.Error("after self-heal, an unmatched mention must NOT count as a bot mention")
	}
	if !f.isBotMentioned(1, func(int) string { return "ou_bot_self" }) {
		t.Error("after self-heal, an exact open_id mention must count")
	}
}

// TestMaybeRefreshBotInfo_RateLimited: a fetch failure stamps the cooldown so
// a burst of degraded mentions doesn't hammer bot/v3/info. R229-ARCH-19 (#1009).
func TestMaybeRefreshBotInfo_RateLimited(t *testing.T) {
	t.Parallel()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t_abc","expire":7200}`))
		case "/open-apis/bot/v3/info":
			atomic.AddInt32(&calls, 1)
			_, _ = w.Write([]byte(`{"code":99991663,"msg":"app access denied"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := New(Config{AppID: "app", AppSecret: "secret"}, nil)
	defer func() { _ = f.Stop() }()
	f.baseURL = server.URL

	// Fire several degraded mentions in quick succession.
	for i := 0; i < 5; i++ {
		f.isBotMentioned(1, func(int) string { return "ou_x" })
	}
	// Let any spawned goroutines settle.
	f.dispatch.Wait()

	if n := atomic.LoadInt32(&calls); n > 1 {
		t.Errorf("bot/v3/info called %d times, want ≤1 (cooldown should rate-limit)", n)
	}
}

// TestFetchBotInfo_Success: end-to-end happy path — token fetch + bot/v3/info
// call + botOpenID population. Uses httptest to serve both endpoints so no
// network is required.
func TestFetchBotInfo_Success(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t_abc","expire":7200}`))
		case "/open-apis/bot/v3/info":
			if got := r.Header.Get("Authorization"); got != "Bearer t_abc" {
				t.Errorf("Authorization header = %q, want Bearer t_abc", got)
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","bot":{"open_id":"ou_bot_test","app_name":"NaozhiBot"}}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := New(Config{AppID: "app", AppSecret: "secret"}, nil)
	defer func() { _ = f.Stop() }()
	f.baseURL = server.URL

	if err := f.fetchBotInfo(context.Background()); err != nil {
		t.Fatalf("fetchBotInfo: %v", err)
	}
	f.botInfoMu.RLock()
	got := f.botOpenID
	f.botInfoMu.RUnlock()
	if got != "ou_bot_test" {
		t.Errorf("botOpenID = %q, want ou_bot_test", got)
	}
}

// TestFetchBotInfo_APIError: non-zero code from bot/v3/info surfaces as
// *APIError so callers can log the upstream code/msg.
func TestFetchBotInfo_APIError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t_abc","expire":7200}`))
		case "/open-apis/bot/v3/info":
			_, _ = w.Write([]byte(`{"code":99991663,"msg":"app access denied"}`))
		}
	}))
	defer server.Close()

	f := New(Config{AppID: "app", AppSecret: "secret"}, nil)
	defer func() { _ = f.Stop() }()
	f.baseURL = server.URL

	err := f.fetchBotInfo(context.Background())
	if err == nil {
		t.Fatal("expected error for non-zero code")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.Code != 99991663 {
		t.Errorf("APIError.Code = %d, want 99991663", apiErr.Code)
	}

	// botOpenID must stay empty on failure so isBotMentioned degrades.
	f.botInfoMu.RLock()
	got := f.botOpenID
	f.botInfoMu.RUnlock()
	if got != "" {
		t.Errorf("botOpenID on error = %q, want empty", got)
	}
}

// TestFetchBotInfo_EmptyOpenID: response code=0 but data.bot.open_id empty
// returns a sentinel error so operators see "why" instead of a silently
// degraded mention check.
func TestFetchBotInfo_EmptyOpenID(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t_abc","expire":7200}`))
		case "/open-apis/bot/v3/info":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","bot":{"open_id":""}}`))
		}
	}))
	defer server.Close()

	f := New(Config{AppID: "app", AppSecret: "secret"}, nil)
	defer func() { _ = f.Stop() }()
	f.baseURL = server.URL

	err := f.fetchBotInfo(context.Background())
	if err == nil {
		t.Fatal("expected error for empty open_id")
	}
	if !strings.Contains(err.Error(), "empty open_id") {
		t.Errorf("error = %v, want substring 'empty open_id'", err)
	}
}
