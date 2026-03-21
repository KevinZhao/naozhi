package feishu

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

// Config holds Feishu app credentials.
type Config struct {
	AppID             string `yaml:"app_id"`
	AppSecret         string `yaml:"app_secret"`
	ConnectionMode    string `yaml:"connection_mode"` // "websocket" (default) | "webhook"
	VerificationToken string `yaml:"verification_token"`
	EncryptKey        string `yaml:"encrypt_key"`
	MaxReplyLen       int    `yaml:"max_reply_length"`
}

// Feishu implements the Platform and RunnablePlatform interfaces.
type Feishu struct {
	cfg         Config
	mode        string // resolved connection mode
	accessToken string
	tokenExpiry time.Time
	tokenMu     sync.Mutex

	// WebSocket lifecycle
	handler platform.MessageHandler
	cancel  context.CancelFunc
	done    chan struct{}
	started bool
}

// New creates a Feishu platform adapter.
func New(cfg Config) *Feishu {
	if cfg.MaxReplyLen <= 0 {
		cfg.MaxReplyLen = 4000
	}
	mode := cfg.ConnectionMode
	if mode == "" {
		mode = "websocket"
	}
	return &Feishu{cfg: cfg, mode: mode}
}

func (f *Feishu) Name() string { return "feishu" }

func (f *Feishu) MaxReplyLength() int { return f.cfg.MaxReplyLen }

func (f *Feishu) Mode() string { return f.mode }

// RegisterRoutes registers webhook routes (only in webhook mode).
func (f *Feishu) RegisterRoutes(mux *http.ServeMux, handler platform.MessageHandler) {
	if f.mode == "webhook" {
		f.registerWebhook(mux, handler)
	}
}

// Start implements RunnablePlatform. Launches WebSocket connection in WS mode.
func (f *Feishu) Start(handler platform.MessageHandler) error {
	if f.started {
		return fmt.Errorf("feishu platform already started")
	}
	f.started = true
	f.handler = handler
	if f.mode == "websocket" {
		slog.Info("feishu using websocket mode (no public IP needed)")
		return f.startWebSocket()
	}
	slog.Info("feishu using webhook mode")
	return nil
}

// Stop implements RunnablePlatform. Stops WebSocket connection.
func (f *Feishu) Stop() error {
	if f.cancel != nil {
		f.cancel()
		<-f.done
	}
	return nil
}

// Reply sends a text message to a Feishu chat.
func (f *Feishu) Reply(ctx context.Context, msg platform.OutgoingMessage) (string, error) {
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("get access token: %w", err)
	}

	content, _ := json.Marshal(map[string]string{"text": msg.Text})
	reqBody, _ := json.Marshal(map[string]interface{}{
		"receive_id": msg.ChatID,
		"msg_type":   "text",
		"content":    string(content),
	})

	req, _ := http.NewRequestWithContext(ctx, "POST",
		"https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type=chat_id",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("send message: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int `json:"code"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("feishu api error: code=%d msg=%s", result.Code, result.Msg)
	}

	return result.Data.MessageID, nil
}

// EditMessage updates an existing Feishu message.
func (f *Feishu) EditMessage(ctx context.Context, msgID string, text string) error {
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	content, _ := json.Marshal(map[string]string{"text": text})
	reqBody, _ := json.Marshal(map[string]interface{}{
		"msg_type": "text",
		"content":  string(content),
	})

	req, _ := http.NewRequestWithContext(ctx, "PUT",
		"https://open.feishu.cn/open-apis/im/v1/messages/"+msgID,
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("edit message: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode edit response: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("feishu edit error: code=%d msg=%s", result.Code, result.Msg)
	}
	return nil
}

// getAccessToken returns a valid tenant access token, refreshing if needed.
func (f *Feishu) getAccessToken(ctx context.Context) (string, error) {
	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()

	if f.accessToken != "" && time.Now().Before(f.tokenExpiry) {
		return f.accessToken, nil
	}

	reqBody, _ := json.Marshal(map[string]string{
		"app_id":     f.cfg.AppID,
		"app_secret": f.cfg.AppSecret,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST",
		"https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request token: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code              int    `json:"code"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("get token error: code=%d", result.Code)
	}

	f.accessToken = result.TenantAccessToken
	f.tokenExpiry = time.Now().Add(time.Duration(result.Expire-60) * time.Second) // refresh 60s early
	return f.accessToken, nil
}

// verifySignature verifies the request signature (for encrypt_key mode).
func verifySignature(timestamp, nonce, encryptKey string, body []byte, signature string) bool {
	if encryptKey == "" {
		return true
	}
	content := timestamp + nonce + encryptKey + string(body)
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", h) == signature
}
