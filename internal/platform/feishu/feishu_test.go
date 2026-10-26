package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func TestVerifySignature(t *testing.T) {
	t.Parallel()
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
		// R224-SEC-2: empty key now fails closed instead of returning true.
		// Callers must gate on cfg.EncryptKey != "" externally; this guards
		// against a future caller forgetting the outer check and silently
		// bypassing signature verification.
		{"empty encrypt key fails closed", timestamp, nonce, "", body, "anything", false},
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

func TestStartAlreadyStarted(t *testing.T) {
	t.Parallel()
	// Webhook mode requires verification_token or encrypt_key — supply an
	// encrypt_key (the secure HMAC path) so Start() reaches the idempotency
	// guard we're exercising here without tripping the SEC-1 token-only gate.
	f := New(Config{AppID: "id", ConnectionMode: "webhook", EncryptKey: "test-key"}, nil)
	noop := func(context.Context, platform.IncomingMessage) {}
	if err := f.Start(noop); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	if err := f.Start(noop); err == nil {
		t.Error("second Start() should return error")
	}
}

// TestStartWebhook_AuthGate: webhook mode needs a credential, and
// verification_token-only mode (no encrypt_key HMAC) is replay/forgery-prone
// so it needs the explicit allow_insecure_webhook opt-in (R250531-SEC-1, #1507).
func TestStartWebhook_AuthGate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		cfg         Config
		wantErr     bool
		errContains string
	}{
		{"missing auth", Config{AppID: "id", ConnectionMode: "webhook"}, true, ""},
		{"token-only without opt-in", Config{AppID: "id", ConnectionMode: "webhook", VerificationToken: "test-token"}, true, "allow_insecure_webhook"},
		{"token-only with opt-in", Config{AppID: "id", ConnectionMode: "webhook", VerificationToken: "test-token", AllowInsecureWebhook: true}, false, ""},
		{"encrypt_key needs no opt-in", Config{AppID: "id", ConnectionMode: "webhook", EncryptKey: "test-key"}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := New(tt.cfg, nil).Start(func(context.Context, platform.IncomingMessage) {})
			if (err != nil) != tt.wantErr {
				t.Fatalf("Start() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
				t.Fatalf("error should mention %q, got: %v", tt.errContains, err)
			}
		})
	}
}

func TestStopNoop(t *testing.T) {
	t.Parallel()
	f := New(Config{AppID: "id", ConnectionMode: "webhook"}, nil)
	if err := f.Stop(); err != nil {
		t.Errorf("Stop() error = %v, want nil", err)
	}
}

func TestStopCancelsDone(t *testing.T) {
	t.Parallel()
	f := New(Config{AppID: "id", ConnectionMode: "webhook"}, nil)
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

// --- Webhook HTTP handler tests ---

// makeWebhookFeishu returns a webhook-mode Feishu. It auto-fills
// VerificationToken if the caller left both auth fields empty, because the
// R67-SEC-9 defense gate now refuses zero-credential handler invocations
// outright — without at least one credential, every subsequent test would
// hit 503. Existing tests that want to drive the gate from the opposite
// direction (TestHandleWebhook_RefusesZeroCredential) construct Feishu
// directly via New() rather than going through this helper.
func makeWebhookFeishu(cfg Config) *Feishu {
	cfg.ConnectionMode = "webhook"
	if cfg.VerificationToken == "" && cfg.EncryptKey == "" {
		cfg.VerificationToken = "test_token"
	}
	return New(cfg, nil)
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
	t.Parallel()
	// makeWebhookFeishu defaults VerificationToken to "test_token" so the
	// R67-SEC-9 defense gate passes; the url_verification body carries the
	// matching token + required timestamp/nonce headers. R67-SEC-9.
	f := makeWebhookFeishu(Config{AppID: "id", AppSecret: "secret"})
	mux := http.NewServeMux()
	f.registerWebhook(mux, func(ctx context.Context, msg platform.IncomingMessage) {})

	body := []byte(`{"type":"url_verification","challenge":"test_challenge_123","token":"test_token"}`)
	req := buildTokenRequest(body)
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
	t.Parallel()
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

// TestHandleWebhook_RefusesZeroCredential is the R67-SEC-9 defense-in-depth
// regression: even if config.validateConfig is bypassed (programmatic
// constructor / test) and a Feishu is wired up with neither VerificationToken
// nor EncryptKey, the handler must refuse inbound webhook requests outright
// with 503 — otherwise the body-parse path below would skip token / signature
// / nonce checks and process arbitrary payloads.
func TestHandleWebhook_RefusesZeroCredential(t *testing.T) {
	t.Parallel()
	// Call New directly (not makeWebhookFeishu) so neither VerificationToken
	// nor EncryptKey is auto-filled — we need the zero-credential state to
	// exercise the defense gate.
	f := New(Config{AppID: "id", AppSecret: "secret", ConnectionMode: "webhook"}, nil)
	mux := http.NewServeMux()
	f.registerWebhook(mux, func(ctx context.Context, msg platform.IncomingMessage) {})

	body := `{"type":"url_verification","challenge":"c"}`
	req := httptest.NewRequest("POST", "/webhook/feishu", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (zero-credential must refuse)", w.Code)
	}
}

func TestWebhook_EmptyTokenBypass(t *testing.T) {
	t.Parallel()
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

// TestWebhook_TokenOnly_RejectsMissingTimestamp pins R245-SEC-5 (#828):
// a webhook configured with VerificationToken but no EncryptKey must still
// reject requests that omit X-Lark-Request-Timestamp. The defense relies on
// the gate at transport_hook.go (`if ts == "" { ... cfg.EncryptKey != "" ||
// cfg.VerificationToken != "" }`) — without it, a stolen verification_token
// could be replayed indefinitely because token-only mode used to skip both
// timestamp freshness AND nonce dedup. This regression test fails closed if
// a future refactor scopes the freshness check to encrypt_key mode only.
func TestWebhook_TokenOnly_RejectsMissingTimestamp(t *testing.T) {
	t.Parallel()
	f := makeWebhookFeishu(Config{
		AppID: "id", AppSecret: "secret",
		VerificationToken: "test_token",
		// EncryptKey deliberately empty: token-only mode.
	})
	mux := http.NewServeMux()
	called := false
	f.registerWebhook(mux, func(ctx context.Context, msg platform.IncomingMessage) {
		called = true
	})

	body := buildV2MessageBody("ev_no_ts", "oc_chat1", "p2p", "replay attempt")
	// Deliberately omit X-Lark-Request-Timestamp so the freshness gate fires.
	req := httptest.NewRequest("POST", "/webhook/feishu", strings.NewReader(string(body)))
	req.Header.Set("X-Lark-Request-Nonce", "any-nonce")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (token-only mode must require timestamp)", w.Code)
	}
	if called {
		t.Error("handler invoked despite missing timestamp")
	}
}

func TestWebhook_SignatureFailure(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	timestamp := fmt.Sprintf("%d", time.Now().Unix())
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
	t.Parallel()
	f := makeWebhookFeishu(Config{AppID: "id", AppSecret: "secret"})
	mux := http.NewServeMux()
	called := false
	f.registerWebhook(mux, func(ctx context.Context, msg platform.IncomingMessage) {
		called = true
	})

	// header.token must match the (auto-filled by makeWebhookFeishu)
	// VerificationToken="test_token" to pass the R67-SEC-9 + token-match gates.
	body := []byte(`{"schema":"2.0","header":{"event_id":"ev_1","event_type":"im.chat.create_v1","token":"test_token"},"event":{}}`)
	req := buildTokenRequest(body)
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
	t.Parallel()
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
			// header.token must match makeWebhookFeishu's auto-filled
			// VerificationToken="test_token" to pass the token gate. R67-SEC-9.
			"token": "test_token",
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
	req := buildTokenRequest(b)
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
	t.Parallel()
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
	t.Parallel()
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

	// buildV2MessageBody sets header.token="test_token" which matches the
	// makeWebhookFeishu auto-filled VerificationToken; timestamp + nonce
	// headers are supplied by buildTokenRequest so the freshness + replay
	// defenses pass. R67-SEC-9.
	body := buildV2MessageBody("ev_valid", "oc_chat1", "group", "hello world")
	req := buildTokenRequest(body)
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

// --- Nonce replay protection tests ---

// buildSignedRequest creates a signed POST request to /webhook/feishu with
// the given timestamp and nonce, computing the HMAC over body.
// buildTokenRequest assembles a webhook request for VerificationToken-only
// mode: adds the timestamp + nonce headers required by the webhook handler's
// freshness and replay defenses. Signature is NOT set (EncryptKey mode covers
// that via buildSignedRequest). Each call uses a unique nonce so repeated
// calls within a single test do not collide with the nonce-dedup cache.
//
// Round 159: tokenNonceCounter must be atomic because Round 158 added
// t.Parallel() to the webhook test suite, which lets multiple tests call
// buildTokenRequest concurrently. A plain int64 increment races.
var tokenNonceCounter atomic.Int64

func buildTokenRequest(body []byte) *http.Request {
	n := tokenNonceCounter.Add(1)
	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	nonce := fmt.Sprintf("tok-nonce-%d-%d", time.Now().UnixNano(), n)
	req := httptest.NewRequest("POST", "/webhook/feishu", strings.NewReader(string(body)))
	req.Header.Set("X-Lark-Request-Timestamp", timestamp)
	req.Header.Set("X-Lark-Request-Nonce", nonce)
	return req
}

func buildSignedRequest(t *testing.T, body []byte, timestamp, nonce, encryptKey string) *http.Request {
	t.Helper()
	sigContent := timestamp + nonce + encryptKey + string(body)
	h := sha256.Sum256([]byte(sigContent))
	sig := fmt.Sprintf("%x", h)
	req := httptest.NewRequest("POST", "/webhook/feishu", strings.NewReader(string(body)))
	req.Header.Set("X-Lark-Request-Timestamp", timestamp)
	req.Header.Set("X-Lark-Request-Nonce", nonce)
	req.Header.Set("X-Lark-Signature", sig)
	return req
}

func TestWebhook_NonceReplay_Rejected(t *testing.T) {
	t.Parallel()
	const encryptKey = "replay_test_key"
	f := makeWebhookFeishu(Config{
		AppID: "id", AppSecret: "secret",
		VerificationToken: "test_token",
		EncryptKey:        encryptKey,
	})
	mux := http.NewServeMux()
	callCount := 0
	done := make(chan struct{}, 1)
	f.registerWebhook(mux, func(ctx context.Context, msg platform.IncomingMessage) {
		callCount++
		done <- struct{}{}
	})

	body := buildV2MessageBody("ev_replay", "oc_chat1", "p2p", "replay me")
	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	nonce := "unique-nonce-abc"

	// First request: must succeed.
	req1 := buildSignedRequest(t, body, timestamp, nonce, encryptKey)
	w1 := httptest.NewRecorder()
	mux.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", w1.Code)
	}
	<-done

	// Second request with identical ts+nonce: must be rejected as replay.
	req2 := buildSignedRequest(t, body, timestamp, nonce, encryptKey)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("replay request status = %d, want 401", w2.Code)
	}
	if callCount != 1 {
		t.Errorf("handler call count = %d, want 1 (replay must not reach handler)", callCount)
	}

	// Same timestamp, different nonce: a legitimate message that must pass.
	req3 := buildSignedRequest(t, body, timestamp, "unique-nonce-def", encryptKey)
	w3 := httptest.NewRecorder()
	mux.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("different-nonce request status = %d, want 200", w3.Code)
	}
	<-done
	if callCount != 2 {
		t.Errorf("handler call count = %d, want 2 (different nonces must both pass)", callCount)
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
