package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func TestVerifySignature(t *testing.T) {
	timestamp := "1234567890"
	nonce := "testnonce"
	encryptKey := "mysecretkey"
	body := []byte(`{"test":"data"}`)

	content := timestamp + nonce + encryptKey + string(body)
	h := sha256.Sum256([]byte(content))
	validSig := fmt.Sprintf("%x", h)

	tests := []struct {
		name       string
		timestamp  string
		nonce      string
		encryptKey string
		body       []byte
		signature  string
		want       bool
	}{
		{"valid signature", timestamp, nonce, encryptKey, body, validSig, true},
		{"invalid signature", timestamp, nonce, encryptKey, body, "bad", false},
		{"empty encrypt key bypasses", timestamp, nonce, "", body, "anything", true},
		{"wrong body", timestamp, nonce, encryptKey, []byte("wrong"), validSig, false},
		{"wrong timestamp", "9999", nonce, encryptKey, body, validSig, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := verifySignature(tt.timestamp, tt.nonce, tt.encryptKey, tt.body, tt.signature)
			if got != tt.want {
				t.Errorf("verifySignature() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Mode selection tests ---

func TestNewDefaultMode(t *testing.T) {
	f := New(Config{AppID: "id", AppSecret: "secret"})
	if f.Mode() != "websocket" {
		t.Errorf("default mode = %q, want websocket", f.Mode())
	}
}

func TestNewWebhookMode(t *testing.T) {
	f := New(Config{AppID: "id", AppSecret: "secret", ConnectionMode: "webhook"})
	if f.Mode() != "webhook" {
		t.Errorf("mode = %q, want webhook", f.Mode())
	}
}

func TestNewExplicitWSMode(t *testing.T) {
	f := New(Config{AppID: "id", AppSecret: "secret", ConnectionMode: "websocket"})
	if f.Mode() != "websocket" {
		t.Errorf("mode = %q, want websocket", f.Mode())
	}
}

func TestDefaultMaxReplyLen(t *testing.T) {
	f := New(Config{AppID: "id"})
	if f.MaxReplyLength() != 4000 {
		t.Errorf("MaxReplyLength() = %d, want 4000", f.MaxReplyLength())
	}
}

func TestCustomMaxReplyLen(t *testing.T) {
	f := New(Config{AppID: "id", MaxReplyLen: 2000})
	if f.MaxReplyLength() != 2000 {
		t.Errorf("MaxReplyLength() = %d, want 2000", f.MaxReplyLength())
	}
}

// Verify Feishu implements RunnablePlatform at compile time.
var _ platform.RunnablePlatform = (*Feishu)(nil)

// --- Start/Stop lifecycle tests ---

func TestStartAlreadyStarted(t *testing.T) {
	f := New(Config{AppID: "id", ConnectionMode: "webhook"})
	noop := func(context.Context, platform.IncomingMessage) {}
	if err := f.Start(noop); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	if err := f.Start(noop); err == nil {
		t.Error("second Start() should return error")
	}
}

func TestStopNoop(t *testing.T) {
	f := New(Config{AppID: "id", ConnectionMode: "webhook"})
	if err := f.Stop(); err != nil {
		t.Errorf("Stop() error = %v, want nil", err)
	}
}

func TestStopCancelsDone(t *testing.T) {
	f := New(Config{AppID: "id", ConnectionMode: "webhook"})
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

	msg, ok := parseSDKEvent(event)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if msg.Platform != "feishu" {
		t.Errorf("Platform = %q, want feishu", msg.Platform)
	}
	if msg.EventID != "ev_123" {
		t.Errorf("EventID = %q, want ev_123", msg.EventID)
	}
	if msg.UserID != "ou_user1" {
		t.Errorf("UserID = %q, want ou_user1", msg.UserID)
	}
	if msg.ChatID != "oc_chat1" {
		t.Errorf("ChatID = %q, want oc_chat1", msg.ChatID)
	}
	if msg.ChatType != "group" {
		t.Errorf("ChatType = %q, want group", msg.ChatType)
	}
	if msg.Text != "hello world" {
		t.Errorf("Text = %q, want 'hello world'", msg.Text)
	}
	if msg.MentionMe {
		t.Error("MentionMe should be false")
	}
}

func TestParseSDKEvent_DirectMessage(t *testing.T) {
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

	msg, ok := parseSDKEvent(event)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if msg.ChatType != "direct" {
		t.Errorf("ChatType = %q, want direct", msg.ChatType)
	}
}

func TestParseSDKEvent_WithMentions(t *testing.T) {
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

	msg, ok := parseSDKEvent(event)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if msg.Text != "do something" {
		t.Errorf("Text = %q, want 'do something'", msg.Text)
	}
	if !msg.MentionMe {
		t.Error("MentionMe should be true")
	}
}

func TestParseSDKEvent_NonText(t *testing.T) {
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Message: &larkim.EventMessage{
				MessageType: strPtr("image"),
				ChatId:      strPtr("oc_chat1"),
				Content:     strPtr(`{}`),
			},
		},
	}
	_, ok := parseSDKEvent(event)
	if ok {
		t.Error("expected ok=false for non-text message")
	}
}

func TestParseSDKEvent_EmptyText(t *testing.T) {
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
	_, ok := parseSDKEvent(event)
	if ok {
		t.Error("expected ok=false for empty text")
	}
}

func TestParseSDKEvent_MentionOnlyText(t *testing.T) {
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
	_, ok := parseSDKEvent(event)
	if ok {
		t.Error("expected ok=false for mention-only text")
	}
}

func TestParseSDKEvent_NilEvent(t *testing.T) {
	_, ok := parseSDKEvent(nil)
	if ok {
		t.Error("expected ok=false for nil event")
	}
}

func TestParseSDKEvent_NilMessage(t *testing.T) {
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{},
	}
	_, ok := parseSDKEvent(event)
	if ok {
		t.Error("expected ok=false for nil message")
	}
}

// --- Webhook HTTP handler tests ---

func makeWebhookFeishu(cfg Config) *Feishu {
	cfg.ConnectionMode = "webhook"
	return New(cfg)
}

func buildV2MessageBody(eventID, chatID, chatType, text string) []byte {
	body := map[string]interface{}{
		"schema": "2.0",
		"header": map[string]interface{}{
			"event_id":   eventID,
			"event_type": "im.message.receive_v1",
			"token":      "test_token",
		},
		"event": map[string]interface{}{
			"sender": map[string]interface{}{
				"sender_id": map[string]interface{}{
					"open_id": "ou_sender",
				},
			},
			"message": map[string]interface{}{
				"message_id":   "msg_1",
				"chat_id":      chatID,
				"chat_type":    chatType,
				"message_type": "text",
				"content":      fmt.Sprintf(`{"text":"%s"}`, text),
			},
		},
	}
	b, _ := json.Marshal(body)
	return b
}

func TestWebhook_Challenge(t *testing.T) {
	f := makeWebhookFeishu(Config{AppID: "id", AppSecret: "secret"})
	mux := http.NewServeMux()
	f.registerWebhook(mux, func(ctx context.Context, msg platform.IncomingMessage) {})

	body := `{"type":"url_verification","challenge":"test_challenge_123"}`
	req := httptest.NewRequest("POST", "/webhook/feishu", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["challenge"] != "test_challenge_123" {
		t.Errorf("challenge = %q, want test_challenge_123", resp["challenge"])
	}
}

func TestWebhook_TokenMismatch(t *testing.T) {
	f := makeWebhookFeishu(Config{
		AppID: "id", AppSecret: "secret",
		VerificationToken: "correct_token",
	})
	mux := http.NewServeMux()
	f.registerWebhook(mux, func(ctx context.Context, msg platform.IncomingMessage) {})

	body := buildV2MessageBody("ev_1", "oc_chat1", "p2p", "hello")
	req := httptest.NewRequest("POST", "/webhook/feishu", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (token in body is 'test_token', configured is 'correct_token')", w.Code)
	}
}

func TestWebhook_EmptyTokenBypass(t *testing.T) {
	f := makeWebhookFeishu(Config{
		AppID: "id", AppSecret: "secret",
		VerificationToken: "correct_token",
	})
	mux := http.NewServeMux()
	f.registerWebhook(mux, func(ctx context.Context, msg platform.IncomingMessage) {})

	// Attacker sends body with no token at top-level or in header
	body := `{"schema":"2.0","header":{"event_id":"ev_1","event_type":"im.message.receive_v1"},"event":{}}`
	req := httptest.NewRequest("POST", "/webhook/feishu", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (missing token should not bypass verification)", w.Code)
	}
}

func TestWebhook_SignatureFailure(t *testing.T) {
	f := makeWebhookFeishu(Config{
		AppID: "id", AppSecret: "secret",
		EncryptKey: "my_secret_key",
	})
	mux := http.NewServeMux()
	f.registerWebhook(mux, func(ctx context.Context, msg platform.IncomingMessage) {})

	body := buildV2MessageBody("ev_1", "oc_chat1", "p2p", "hello")
	req := httptest.NewRequest("POST", "/webhook/feishu", strings.NewReader(string(body)))
	req.Header.Set("X-Lark-Request-Timestamp", "12345")
	req.Header.Set("X-Lark-Request-Nonce", "nonce")
	req.Header.Set("X-Lark-Signature", "bad_signature")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestWebhook_ValidSignature(t *testing.T) {
	encryptKey := "my_secret_key"
	f := makeWebhookFeishu(Config{
		AppID: "id", AppSecret: "secret",
		VerificationToken: "test_token",
		EncryptKey:        encryptKey,
	})
	mux := http.NewServeMux()
	var received platform.IncomingMessage
	var mu sync.Mutex
	done := make(chan struct{})
	f.registerWebhook(mux, func(ctx context.Context, msg platform.IncomingMessage) {
		mu.Lock()
		received = msg
		mu.Unlock()
		close(done)
	})

	body := buildV2MessageBody("ev_sig", "oc_chat1", "p2p", "signed msg")
	timestamp := "12345"
	nonce := "nonce123"
	sigContent := timestamp + nonce + encryptKey + string(body)
	h := sha256.Sum256([]byte(sigContent))
	sig := fmt.Sprintf("%x", h)

	req := httptest.NewRequest("POST", "/webhook/feishu", strings.NewReader(string(body)))
	req.Header.Set("X-Lark-Request-Timestamp", timestamp)
	req.Header.Set("X-Lark-Request-Nonce", nonce)
	req.Header.Set("X-Lark-Signature", sig)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	<-done
	mu.Lock()
	defer mu.Unlock()
	if received.Text != "signed msg" {
		t.Errorf("received text = %q, want 'signed msg'", received.Text)
	}
	if received.EventID != "ev_sig" {
		t.Errorf("received eventID = %q, want ev_sig", received.EventID)
	}
}

func TestWebhook_NonMessageEvent(t *testing.T) {
	f := makeWebhookFeishu(Config{AppID: "id", AppSecret: "secret"})
	mux := http.NewServeMux()
	called := false
	f.registerWebhook(mux, func(ctx context.Context, msg platform.IncomingMessage) {
		called = true
	})

	body := `{"schema":"2.0","header":{"event_id":"ev_1","event_type":"im.chat.create_v1","token":""},"event":{}}`
	req := httptest.NewRequest("POST", "/webhook/feishu", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if called {
		t.Error("handler should not be called for non-message events")
	}
}

func TestWebhook_NonTextMessage(t *testing.T) {
	f := makeWebhookFeishu(Config{AppID: "id", AppSecret: "secret"})
	mux := http.NewServeMux()
	called := false
	f.registerWebhook(mux, func(ctx context.Context, msg platform.IncomingMessage) {
		called = true
	})

	body := map[string]interface{}{
		"schema": "2.0",
		"header": map[string]interface{}{
			"event_id":   "ev_1",
			"event_type": "im.message.receive_v1",
			"token":      "",
		},
		"event": map[string]interface{}{
			"sender": map[string]interface{}{
				"sender_id": map[string]interface{}{"open_id": "ou_1"},
			},
			"message": map[string]interface{}{
				"message_type": "image",
				"chat_id":      "oc_1",
				"content":      "{}",
			},
		},
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/webhook/feishu", strings.NewReader(string(b)))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if called {
		t.Error("handler should not be called for non-text messages")
	}
}

func TestWebhook_InvalidJSON(t *testing.T) {
	f := makeWebhookFeishu(Config{AppID: "id", AppSecret: "secret"})
	mux := http.NewServeMux()
	f.registerWebhook(mux, func(ctx context.Context, msg platform.IncomingMessage) {})

	req := httptest.NewRequest("POST", "/webhook/feishu", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestWebhook_ValidMessage(t *testing.T) {
	f := makeWebhookFeishu(Config{AppID: "id", AppSecret: "secret"})
	mux := http.NewServeMux()
	var received platform.IncomingMessage
	var mu sync.Mutex
	done := make(chan struct{})
	f.registerWebhook(mux, func(ctx context.Context, msg platform.IncomingMessage) {
		mu.Lock()
		received = msg
		mu.Unlock()
		close(done)
	})

	body := buildV2MessageBody("ev_valid", "oc_chat1", "group", "hello world")
	req := httptest.NewRequest("POST", "/webhook/feishu", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	<-done
	mu.Lock()
	defer mu.Unlock()
	if received.Platform != "feishu" {
		t.Errorf("platform = %q, want feishu", received.Platform)
	}
	if received.EventID != "ev_valid" {
		t.Errorf("eventID = %q, want ev_valid", received.EventID)
	}
	if received.Text != "hello world" {
		t.Errorf("text = %q, want 'hello world'", received.Text)
	}
	if received.ChatType != "group" {
		t.Errorf("chatType = %q, want group", received.ChatType)
	}
	if received.UserID != "ou_sender" {
		t.Errorf("userID = %q, want ou_sender", received.UserID)
	}
}
