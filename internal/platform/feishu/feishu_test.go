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

func TestDefaultMaxReplyLen(t *testing.T) {
	t.Parallel()
	f := New(Config{AppID: "id"}, nil)
	if f.MaxReplyLength() != 4000 {
		t.Errorf("MaxReplyLength() = %d, want 4000", f.MaxReplyLength())
	}
}

func TestCustomMaxReplyLen(t *testing.T) {
	t.Parallel()
	f := New(Config{AppID: "id", MaxReplyLen: 2000}, nil)
	if f.MaxReplyLength() != 2000 {
		t.Errorf("MaxReplyLength() = %d, want 2000", f.MaxReplyLength())
	}
}

// Verify Feishu implements RunnablePlatform at compile time.
var _ platform.RunnablePlatform = (*Feishu)(nil)

// --- Start/Stop lifecycle tests ---

func TestStopNoop(t *testing.T) {
	t.Parallel()
	f := New(Config{AppID: "id"}, nil)
	if err := f.Stop(); err != nil {
		t.Errorf("Stop() error = %v, want nil", err)
	}
}

func TestStopCancelsDone(t *testing.T) {
	t.Parallel()
	f := New(Config{AppID: "id"}, nil)
	// Simulate a started WS by manually setting cancel/done
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.done = make(chan struct{})
	go func() {
		<-ctx.Done()
		close(f.done)
	}()

	if err := f.Stop(); err != nil {
		t.Errorf("Stop() error = %v", err)
	}
	// done channel should be closed
	select {
	case <-f.done:
	default:
		t.Error("done channel should be closed after Stop()")
	}
}

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

func TestParseSDKEvent_DirectMessage(t *testing.T) {
	t.Parallel()
	event := &larkim.P2MessageReceiveV1{
		EventV2Base: &larkevent.EventV2Base{
			Header: &larkevent.EventHeader{EventID: "ev_456"},
		},
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: strPtr("ou_user2")},
			},
			Message: &larkim.EventMessage{
				MessageType: strPtr("text"),
				ChatId:      strPtr("oc_chat2"),
				ChatType:    strPtr("p2p"),
				Content:     strPtr(`{"text":"hi"}`),
			},
		},
	}

	pe, ok := (&Feishu{}).parseSDKEvent(event)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if pe.Msg.ChatType != "direct" {
		t.Errorf("ChatType = %q, want direct", pe.Msg.ChatType)
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

func TestParseSDKEvent_ImageMessage(t *testing.T) {
	t.Parallel()
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: strPtr("ou_user1")},
			},
			Message: &larkim.EventMessage{
				MessageType: strPtr("image"),
				ChatId:      strPtr("oc_chat1"),
				Content:     strPtr(`{"image_key":"img_v3_xxx"}`),
			},
		},
	}
	pe, ok := (&Feishu{}).parseSDKEvent(event)
	if !ok {
		t.Fatal("expected ok=true for image message")
	}
	if pe.MediaKey != "img_v3_xxx" {
		t.Errorf("MediaKey = %q, want img_v3_xxx", pe.MediaKey)
	}
	if pe.Msg.Text != "" {
		t.Errorf("Text = %q, want empty for image message", pe.Msg.Text)
	}
}

func TestParseSDKEvent_ImageMessage_EmptyKey(t *testing.T) {
	t.Parallel()
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: strPtr("ou_user1")},
			},
			Message: &larkim.EventMessage{
				MessageType: strPtr("image"),
				ChatId:      strPtr("oc_chat1"),
				Content:     strPtr(`{"image_key":""}`),
			},
		},
	}
	_, ok := (&Feishu{}).parseSDKEvent(event)
	if ok {
		t.Error("expected ok=false for image message with empty image_key")
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

func TestParseSDKEvent_EmptyText(t *testing.T) {
	t.Parallel()
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: strPtr("ou_user1")},
			},
			Message: &larkim.EventMessage{
				MessageType: strPtr("text"),
				ChatId:      strPtr("oc_chat1"),
				Content:     strPtr(`{"text":"  "}`),
			},
		},
	}
	_, ok := (&Feishu{}).parseSDKEvent(event)
	if ok {
		t.Error("expected ok=false for empty text")
	}
}

func TestParseSDKEvent_MentionOnlyText(t *testing.T) {
	t.Parallel()
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: strPtr("ou_user1")},
			},
			Message: &larkim.EventMessage{
				MessageType: strPtr("text"),
				ChatId:      strPtr("oc_chat1"),
				ChatType:    strPtr("group"),
				Content:     strPtr(`{"text":"@_user_1"}`),
				Mentions: []*larkim.MentionEvent{
					{Key: strPtr("@_user_1"), Name: strPtr("Bot")},
				},
			},
		},
	}
	_, ok := (&Feishu{}).parseSDKEvent(event)
	if ok {
		t.Error("expected ok=false for mention-only text")
	}
}

func TestParseSDKEvent_NilEvent(t *testing.T) {
	t.Parallel()
	_, ok := (&Feishu{}).parseSDKEvent(nil)
	if ok {
		t.Error("expected ok=false for nil event")
	}
}

func TestParseSDKEvent_NilMessage(t *testing.T) {
	t.Parallel()
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{},
	}
	_, ok := (&Feishu{}).parseSDKEvent(event)
	if ok {
		t.Error("expected ok=false for nil message")
	}
}

func TestParseSDKEvent_AudioMessage(t *testing.T) {
	t.Parallel()
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: strPtr("ou_user1")},
			},
			Message: &larkim.EventMessage{
				MessageType: strPtr("audio"),
				MessageId:   strPtr("msg_audio_1"),
				ChatId:      strPtr("oc_chat1"),
				Content:     strPtr(`{"file_key":"file_v3_audio_xxx"}`),
			},
		},
	}
	pe, ok := (&Feishu{}).parseSDKEvent(event)
	if !ok {
		t.Fatal("expected ok=true for audio message")
	}
	if pe.MediaType != "audio" {
		t.Errorf("MediaType = %q, want audio", pe.MediaType)
	}
	if pe.MediaKey != "file_v3_audio_xxx" {
		t.Errorf("MediaKey = %q, want file_v3_audio_xxx", pe.MediaKey)
	}
	if pe.MessageID != "msg_audio_1" {
		t.Errorf("MessageID = %q, want msg_audio_1", pe.MessageID)
	}
}

func TestParseSDKEvent_AudioMessage_EmptyKey(t *testing.T) {
	t.Parallel()
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: strPtr("ou_user1")},
			},
			Message: &larkim.EventMessage{
				MessageType: strPtr("audio"),
				ChatId:      strPtr("oc_chat1"),
				Content:     strPtr(`{"file_key":""}`),
			},
		},
	}
	_, ok := (&Feishu{}).parseSDKEvent(event)
	if ok {
		t.Error("expected ok=false for audio message with empty file_key")
	}
}

// --- MentionMe precision (isBotMentioned) + fetchBotInfo tests ---

// TestIsBotMentioned_ExactMatch: botOpenID is known, at least one mention
// matches → true.
func TestIsBotMentioned_ExactMatch(t *testing.T) {
	t.Parallel()
	f := &Feishu{botOpenID: "ou_bot_self"}
	openIDs := []string{"ou_alice", "ou_bot_self", "ou_bob"}
	got := f.isBotMentioned(len(openIDs), func(i int) string { return openIDs[i] })
	if !got {
		t.Error("expected true when bot is among the mentions")
	}
}

// TestIsBotMentioned_NoMatch: botOpenID is known but no mention matches → false.
// This is the core precision fix — the old code returned true here.
func TestIsBotMentioned_NoMatch(t *testing.T) {
	t.Parallel()
	f := &Feishu{botOpenID: "ou_bot_self"}
	openIDs := []string{"ou_alice", "ou_bob"}
	got := f.isBotMentioned(len(openIDs), func(i int) string { return openIDs[i] })
	if got {
		t.Error("expected false when bot is NOT among the mentions")
	}
}

// TestIsBotMentioned_DegradesWhenBotIDUnknown: botOpenID empty (fetchBotInfo
// failed or hasn't run) → any mention counts, any empty list is false.
// Preserves legacy behaviour for DM-heavy users.
func TestIsBotMentioned_DegradesWhenBotIDUnknown(t *testing.T) {
	t.Parallel()
	f := &Feishu{} // botOpenID=""
	if !f.isBotMentioned(3, func(i int) string { return "" }) {
		t.Error("expected degraded path to return true when any mention exists")
	}
	if f.isBotMentioned(0, func(i int) string { return "" }) {
		t.Error("expected false for zero mentions even in degraded path")
	}
}

// TestIsBotMentioned_EmptyOpenIDInPayload: botOpenID known but a mention
// carries empty open_id (older Feishu schema without id.open_id field) →
// skip that mention, keep scanning. Matches real-world rollout where some
// payloads decode with empty ID.
func TestIsBotMentioned_EmptyOpenIDInPayload(t *testing.T) {
	t.Parallel()
	f := &Feishu{botOpenID: "ou_bot_self"}
	openIDs := []string{"", "ou_bot_self"}
	if !f.isBotMentioned(len(openIDs), func(i int) string { return openIDs[i] }) {
		t.Error("expected true when bot appears after an empty-ID entry")
	}
	onlyEmpty := []string{"", ""}
	if f.isBotMentioned(len(onlyEmpty), func(i int) string { return onlyEmpty[i] }) {
		t.Error("expected false when all IDs are empty and bot ID is known")
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
