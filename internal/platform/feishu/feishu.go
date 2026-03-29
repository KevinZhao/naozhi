package feishu

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
	"golang.org/x/sync/singleflight"
)

var feishuHTTPClient = &http.Client{Timeout: 10 * time.Second}

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
	baseURL     string // API base URL (overridable for testing)
	accessToken string
	tokenExpiry time.Time
	tokenMu     sync.RWMutex
	tokenGroup  singleflight.Group

	// WebSocket lifecycle
	handler platform.MessageHandler
	cancel  context.CancelFunc
	done    chan struct{}
	wg      sync.WaitGroup // tracks in-flight message handler goroutines
	startMu sync.Mutex
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
	return &Feishu{cfg: cfg, mode: mode, baseURL: "https://open.feishu.cn"}
}

func (f *Feishu) Name() string { return "feishu" }

func (f *Feishu) MaxReplyLength() int { return f.cfg.MaxReplyLen }

func (f *Feishu) SupportsInterimMessages() bool { return true }

func (f *Feishu) Mode() string { return f.mode }

// RegisterRoutes registers webhook routes (only in webhook mode).
func (f *Feishu) RegisterRoutes(mux *http.ServeMux, handler platform.MessageHandler) {
	if f.mode == "webhook" {
		f.registerWebhook(mux, handler)
	}
}

// Start implements RunnablePlatform. Launches WebSocket connection in WS mode.
func (f *Feishu) Start(handler platform.MessageHandler) error {
	f.startMu.Lock()
	if f.started {
		f.startMu.Unlock()
		return fmt.Errorf("feishu platform already started")
	}
	f.started = true
	f.startMu.Unlock()

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
		f.wg.Wait() // wait for in-flight message handlers to finish
		<-f.done
	}
	return nil
}

// Reply sends a message to a Feishu chat. Handles text and/or images.
func (f *Feishu) Reply(ctx context.Context, msg platform.OutgoingMessage) (string, error) {
	var lastMsgID string

	// Send text message
	if msg.Text != "" {
		id, err := f.sendText(ctx, msg.ChatID, msg.Text)
		if err != nil {
			return "", err
		}
		lastMsgID = id
	}

	// Send image messages
	for _, img := range msg.Images {
		id, err := f.sendImage(ctx, msg.ChatID, img)
		if err != nil {
			slog.Warn("feishu send image failed", "err", err)
			continue
		}
		lastMsgID = id
	}

	return lastMsgID, nil
}

func (f *Feishu) sendText(ctx context.Context, chatID, text string) (string, error) {
	if hasMarkdown(text) {
		return f.sendCard(ctx, chatID, text)
	}
	return f.sendPlainText(ctx, chatID, text)
}

// hasMarkdown detects whether text contains markdown formatting worth rendering.
func hasMarkdown(text string) bool {
	if strings.Contains(text, "```") {
		return true
	}
	remaining := text
	for i := 0; remaining != "" && i < 50; i++ {
		line := remaining
		if idx := strings.IndexByte(remaining, '\n'); idx >= 0 {
			line = remaining[:idx]
			remaining = remaining[idx+1:]
		} else {
			remaining = ""
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "## ") ||
			strings.HasPrefix(trimmed, "### ") || strings.HasPrefix(trimmed, "- ") ||
			strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "1. ") ||
			strings.HasPrefix(trimmed, "> ") || strings.HasPrefix(trimmed, "| ") {
			return true
		}
	}
	if strings.Contains(text, "**") || strings.Contains(text, "__") {
		return true
	}
	return false
}

// sendCard sends a Feishu interactive card with markdown content.
func (f *Feishu) sendCard(ctx context.Context, chatID, text string) (string, error) {
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("get access token: %w", err)
	}

	card := map[string]interface{}{
		"elements": []interface{}{
			map[string]interface{}{
				"tag":     "markdown",
				"content": text,
			},
		},
	}
	cardJSON, err := json.Marshal(card)
	if err != nil {
		return "", fmt.Errorf("marshal card: %w", err)
	}
	reqBody, err := json.Marshal(map[string]interface{}{
		"receive_id": chatID,
		"msg_type":   "interactive",
		"content":    string(cardJSON),
	})
	if err != nil {
		return "", fmt.Errorf("marshal request body: %w", err)
	}

	return f.postMessage(ctx, token, reqBody)
}

func (f *Feishu) sendPlainText(ctx context.Context, chatID, text string) (string, error) {
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("get access token: %w", err)
	}

	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return "", fmt.Errorf("marshal content: %w", err)
	}
	reqBody, err := json.Marshal(map[string]interface{}{
		"receive_id": chatID,
		"msg_type":   "text",
		"content":    string(content),
	})
	if err != nil {
		return "", fmt.Errorf("marshal request body: %w", err)
	}

	return f.postMessage(ctx, token, reqBody)
}

// postMessage sends a prepared message payload to the Feishu API.
func (f *Feishu) postMessage(ctx context.Context, token string, reqBody []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST",
		f.baseURL+"/open-apis/im/v1/messages?receive_id_type=chat_id",
		bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := feishuHTTPClient.Do(req)
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

func (f *Feishu) sendImage(ctx context.Context, chatID string, img platform.Image) (string, error) {
	imageKey, err := f.uploadImage(ctx, img.Data, img.MimeType)
	if err != nil {
		return "", fmt.Errorf("upload image: %w", err)
	}

	token, err := f.getAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("get access token: %w", err)
	}

	content, err := json.Marshal(map[string]string{"image_key": imageKey})
	if err != nil {
		return "", fmt.Errorf("marshal content: %w", err)
	}
	reqBody, err := json.Marshal(map[string]interface{}{
		"receive_id": chatID,
		"msg_type":   "image",
		"content":    string(content),
	})
	if err != nil {
		return "", fmt.Errorf("marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		f.baseURL+"/open-apis/im/v1/messages?receive_id_type=chat_id",
		bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := feishuHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("send image message: %w", err)
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
		return "", fmt.Errorf("feishu send image error: code=%d msg=%s", result.Code, result.Msg)
	}
	return result.Data.MessageID, nil
}

// DownloadImage downloads an image from a message via Feishu API.
func (f *Feishu) DownloadImage(ctx context.Context, messageID, fileKey string) ([]byte, string, error) {
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("get access token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET",
		f.baseURL+"/open-apis/im/v1/messages/"+messageID+"/resources/"+fileKey+"?type=image", nil)
	if err != nil {
		return nil, "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := feishuHTTPClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf("download image: status %d, body: %s", resp.StatusCode, body)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024)) // 10MB max
	if err != nil {
		return nil, "", fmt.Errorf("read image body: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	// Strip MIME parameters (e.g., "image/png; name=file.png" → "image/png")
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = strings.TrimSpace(contentType[:i])
	}
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = "image/png"
	}
	return data, contentType, nil
}

// uploadImage uploads image data to Feishu and returns the image_key.
func (f *Feishu) uploadImage(ctx context.Context, data []byte, mimeType string) (string, error) {
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("get access token: %w", err)
	}

	// Derive filename extension from MIME type
	filename := "image.png"
	switch mimeType {
	case "image/jpeg":
		filename = "image.jpg"
	case "image/gif":
		filename = "image.gif"
	case "image/webp":
		filename = "image.webp"
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("image_type", "message"); err != nil {
		return "", fmt.Errorf("write image_type field: %w", err)
	}
	part, err := w.CreateFormFile("image", filename)
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("write image data: %w", err)
	}
	w.Close()

	req, err := http.NewRequestWithContext(ctx, "POST",
		f.baseURL+"/open-apis/im/v1/images", &buf)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := feishuHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload image: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int `json:"code"`
		Data struct {
			ImageKey string `json:"image_key"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode upload response: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("feishu upload error: code=%d msg=%s", result.Code, result.Msg)
	}
	return result.Data.ImageKey, nil
}

// EditMessage updates an existing Feishu message.
// Returns an error for markdown content (card messages can't replace text messages),
// letting the caller fall back to sending a new card message.
func (f *Feishu) EditMessage(ctx context.Context, msgID string, text string) error {
	if hasMarkdown(text) {
		return fmt.Errorf("markdown content requires card message, cannot edit text message")
	}

	token, err := f.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("marshal content: %w", err)
	}
	reqBody, err := json.Marshal(map[string]interface{}{
		"msg_type": "text",
		"content":  string(content),
	})
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "PUT",
		f.baseURL+"/open-apis/im/v1/messages/"+msgID,
		bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := feishuHTTPClient.Do(req)
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
// Uses singleflight to merge concurrent refresh requests.
func (f *Feishu) getAccessToken(ctx context.Context) (string, error) {
	// Fast path: RLock to check cached token
	f.tokenMu.RLock()
	if f.accessToken != "" && time.Now().Before(f.tokenExpiry) {
		token := f.accessToken
		f.tokenMu.RUnlock()
		return token, nil
	}
	f.tokenMu.RUnlock()

	// Slow path: singleflight merges concurrent refresh calls
	v, err, _ := f.tokenGroup.Do("token", func() (any, error) {
		// Double-check under read lock
		f.tokenMu.RLock()
		if f.accessToken != "" && time.Now().Before(f.tokenExpiry) {
			token := f.accessToken
			f.tokenMu.RUnlock()
			return token, nil
		}
		f.tokenMu.RUnlock()

		reqBody, err := json.Marshal(map[string]string{
			"app_id":     f.cfg.AppID,
			"app_secret": f.cfg.AppSecret,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal token request: %w", err)
		}

		refreshCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(refreshCtx, "POST",
			f.baseURL+"/open-apis/auth/v3/tenant_access_token/internal",
			bytes.NewReader(reqBody))
		if err != nil {
			return nil, fmt.Errorf("create token request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := feishuHTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("request token: %w", err)
		}
		defer resp.Body.Close()

		var result struct {
			Code              int    `json:"code"`
			TenantAccessToken string `json:"tenant_access_token"`
			Expire            int    `json:"expire"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, fmt.Errorf("decode token response: %w", err)
		}
		if result.Code != 0 {
			return nil, fmt.Errorf("get token error: code=%d", result.Code)
		}

		f.tokenMu.Lock()
		f.accessToken = result.TenantAccessToken
		f.tokenExpiry = time.Now().Add(time.Duration(result.Expire-60) * time.Second)
		f.tokenMu.Unlock()

		return result.TenantAccessToken, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// verifySignature verifies the request signature (for encrypt_key mode).
func verifySignature(timestamp, nonce, encryptKey string, body []byte, signature string) bool {
	if encryptKey == "" {
		return true
	}
	content := timestamp + nonce + encryptKey + string(body)
	h := sha256.Sum256([]byte(content))
	computed := fmt.Sprintf("%x", h)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(signature)) == 1
}
