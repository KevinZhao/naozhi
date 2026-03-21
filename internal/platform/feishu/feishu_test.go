package feishu

import (
	"crypto/sha256"
	"fmt"
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

func TestStopNoop(t *testing.T) {
	f := New(Config{AppID: "id", ConnectionMode: "webhook"})
	if err := f.Stop(); err != nil {
		t.Errorf("Stop() error = %v, want nil", err)
	}
}
