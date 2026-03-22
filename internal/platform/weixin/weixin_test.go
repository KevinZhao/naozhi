package weixin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

func TestNew_Defaults(t *testing.T) {
	w := New(Config{Token: "tok"})
	if w.Name() != "weixin" {
		t.Errorf("Name() = %q, want %q", w.Name(), "weixin")
	}
	if w.MaxReplyLength() != 4000 {
		t.Errorf("MaxReplyLength() = %d, want 4000", w.MaxReplyLength())
	}
}

func TestNew_CustomMaxReplyLen(t *testing.T) {
	w := New(Config{Token: "tok", MaxReplyLen: 2000})
	if w.MaxReplyLength() != 2000 {
		t.Errorf("MaxReplyLength() = %d, want 2000", w.MaxReplyLength())
	}
}

func TestExtractText(t *testing.T) {
	tests := []struct {
		name string
		msg  weixinMessage
		want string
	}{
		{
			name: "text message",
			msg: weixinMessage{
				ItemList: []messageItem{
					{Type: msgItemTypeText, TextItem: &textItem{Text: "hello"}},
				},
			},
			want: "hello",
		},
		{
			name: "no text item",
			msg: weixinMessage{
				ItemList: []messageItem{
					{Type: msgItemTypeImage},
				},
			},
			want: "",
		},
		{
			name: "empty item list",
			msg:  weixinMessage{},
			want: "",
		},
		{
			name: "nil text item",
			msg: weixinMessage{
				ItemList: []messageItem{
					{Type: msgItemTypeText, TextItem: nil},
				},
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractText(tt.msg)
			if got != tt.want {
				t.Errorf("extractText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStartStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return empty response for getUpdates
		json.NewEncoder(w).Encode(getUpdatesResp{Ret: 0})
	}))
	defer srv.Close()

	w := New(Config{Token: "tok", BaseURL: srv.URL})

	handler := func(_ context.Context, _ platform.IncomingMessage) {}

	if err := w.Start(handler); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Double start should fail
	if err := w.Start(handler); err == nil {
		t.Fatal("expected error on double Start()")
	}

	if err := w.Stop(); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
}

func TestReply_NoContextToken(t *testing.T) {
	w := New(Config{Token: "tok"})
	_, err := w.Reply(context.Background(), platform.OutgoingMessage{
		ChatID: "user123",
		Text:   "hello",
	})
	if err == nil {
		t.Fatal("expected error when no context_token cached")
	}
}

func TestReply_WithContextToken(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		var req sendMessageReq
		json.NewDecoder(r.Body).Decode(&req)

		if req.Msg.ToUserID != "user123" {
			http.Error(w, "bad to_user_id", 400)
			return
		}
		if req.Msg.ContextToken == "" {
			http.Error(w, "missing context_token", 400)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":0}`))
	}))
	defer srv.Close()

	w := New(Config{Token: "tok", BaseURL: srv.URL})
	// Pre-cache a context token
	w.contextTokens.Store("user123", "ctx-tok-abc")

	msgID, err := w.Reply(context.Background(), platform.OutgoingMessage{
		ChatID: "user123",
		Text:   "hi there",
	})
	if err != nil {
		t.Fatalf("Reply() error: %v", err)
	}
	if msgID == "" {
		t.Error("expected non-empty msgID")
	}
	if !called.Load() {
		t.Error("sendMessage was not called")
	}
}

func TestPollLoop_ReceivesMessages(t *testing.T) {
	pollCount := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := pollCount.Add(1)
		if n == 1 {
			// First poll: return a message
			json.NewEncoder(w).Encode(getUpdatesResp{
				Ret: 0,
				Msgs: []weixinMessage{
					{
						MessageID:    42,
						FromUserID:   "alice",
						MessageType:  msgTypeUser,
						ContextToken: "ctx-1",
						ItemList: []messageItem{
							{Type: msgItemTypeText, TextItem: &textItem{Text: "hello bot"}},
						},
					},
				},
				GetUpdatesBuf: "cursor-1",
			})
		} else {
			// Subsequent polls: empty, slow to simulate long-poll
			time.Sleep(100 * time.Millisecond)
			json.NewEncoder(w).Encode(getUpdatesResp{Ret: 0, GetUpdatesBuf: "cursor-1"})
		}
	}))
	defer srv.Close()

	w := New(Config{Token: "tok", BaseURL: srv.URL})

	var received atomic.Int32
	var receivedMsg platform.IncomingMessage
	var mu sync.Mutex

	handler := func(_ context.Context, msg platform.IncomingMessage) {
		mu.Lock()
		receivedMsg = msg
		mu.Unlock()
		received.Add(1)
	}

	if err := w.Start(handler); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer w.Stop()

	// Wait for message to be received
	deadline := time.After(3 * time.Second)
	for received.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for message")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	mu.Lock()
	msg := receivedMsg
	mu.Unlock()

	if msg.Platform != "weixin" {
		t.Errorf("Platform = %q, want %q", msg.Platform, "weixin")
	}
	if msg.UserID != "alice" {
		t.Errorf("UserID = %q, want %q", msg.UserID, "alice")
	}
	if msg.Text != "hello bot" {
		t.Errorf("Text = %q, want %q", msg.Text, "hello bot")
	}
	if msg.ChatType != "direct" {
		t.Errorf("ChatType = %q, want %q", msg.ChatType, "direct")
	}

	// Verify context_token was cached
	ct, ok := w.contextTokens.Load("alice")
	if !ok || ct.(string) != "ctx-1" {
		t.Errorf("context_token not cached, got %v", ct)
	}
}

func TestPollLoop_SkipsBotMessages(t *testing.T) {
	pollCount := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := pollCount.Add(1)
		if n == 1 {
			json.NewEncoder(w).Encode(getUpdatesResp{
				Ret: 0,
				Msgs: []weixinMessage{
					{
						MessageID:   1,
						FromUserID:  "bot",
						MessageType: msgTypeBOT, // bot message, should be skipped
						ItemList: []messageItem{
							{Type: msgItemTypeText, TextItem: &textItem{Text: "bot reply"}},
						},
					},
				},
			})
		} else {
			time.Sleep(100 * time.Millisecond)
			json.NewEncoder(w).Encode(getUpdatesResp{Ret: 0})
		}
	}))
	defer srv.Close()

	w := New(Config{Token: "tok", BaseURL: srv.URL})
	var received atomic.Int32
	handler := func(_ context.Context, _ platform.IncomingMessage) {
		received.Add(1)
	}

	if err := w.Start(handler); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer w.Stop()

	// Wait a bit — no message should be received
	time.Sleep(500 * time.Millisecond)
	if received.Load() != 0 {
		t.Errorf("received %d messages, expected 0 (bot messages should be skipped)", received.Load())
	}
}

func TestAPIClient_GetUpdates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ilink/bot/getupdates" {
			http.Error(w, "not found", 404)
			return
		}
		if r.Header.Get("AuthorizationType") != "ilink_bot_token" {
			http.Error(w, "bad auth type", 401)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "bad auth", 401)
			return
		}
		json.NewEncoder(w).Encode(getUpdatesResp{
			Ret:           0,
			GetUpdatesBuf: "new-cursor",
		})
	}))
	defer srv.Close()

	api := newAPIClient(srv.URL, "test-token")
	resp, err := api.getUpdates(context.Background(), "")
	if err != nil {
		t.Fatalf("getUpdates error: %v", err)
	}
	if resp.Ret != 0 {
		t.Errorf("ret = %d, want 0", resp.Ret)
	}
	if resp.GetUpdatesBuf != "new-cursor" {
		t.Errorf("cursor = %q, want %q", resp.GetUpdatesBuf, "new-cursor")
	}
}

func TestAPIClient_SendMessage(t *testing.T) {
	var received sendMessageReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ilink/bot/sendmessage" {
			http.Error(w, "not found", 404)
			return
		}
		json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":0}`))
	}))
	defer srv.Close()

	api := newAPIClient(srv.URL, "test-token")
	err := api.sendMessage(context.Background(), "user1", "hello", "ctx-tok")
	if err != nil {
		t.Fatalf("sendMessage error: %v", err)
	}
	if received.Msg.ToUserID != "user1" {
		t.Errorf("to_user_id = %q, want %q", received.Msg.ToUserID, "user1")
	}
	if received.Msg.ContextToken != "ctx-tok" {
		t.Errorf("context_token = %q, want %q", received.Msg.ContextToken, "ctx-tok")
	}
	if len(received.Msg.ItemList) != 1 || received.Msg.ItemList[0].TextItem.Text != "hello" {
		t.Errorf("unexpected item_list: %+v", received.Msg.ItemList)
	}
}

func TestEditMessage_Noop(t *testing.T) {
	w := New(Config{Token: "tok"})
	err := w.EditMessage(context.Background(), "any-id", "new text")
	if err != nil {
		t.Errorf("EditMessage should be no-op, got error: %v", err)
	}
}

func TestRandomWechatUIN(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 10; i++ {
		v := randomWechatUIN()
		if v == "" {
			t.Fatal("empty UIN")
		}
		if seen[v] {
			t.Errorf("duplicate UIN: %s", v)
		}
		seen[v] = true
	}
}
