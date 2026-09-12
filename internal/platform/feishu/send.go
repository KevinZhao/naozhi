package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

// send.go — outbound text: Reply, the markdown-card body it builds, the POST
// that carries it, the error reply, and EditMessage. Extracted from feishu.go
// (J10 of #2548). Image sending lives in media.go, which shares postMessage's
// token handling but not its body shape.

func (f *Feishu) Reply(ctx context.Context, msg platform.OutgoingMessage) (string, error) {
	var lastMsgID string

	if msg.Text != "" {
		id, err := f.sendText(ctx, msg.ChatID, msg.Text)
		if err != nil {
			// A token-expired error invalidates the cache so the retry has a fresh token.
			return "", f.maybeInvalidateOnTokenError(err)
		}
		lastMsgID = id
	}

	// Non-token image failures are log-and-continue (earlier parts landed),
	// but a token-invalidation error must PROPAGATE so ReplyWithRetry can
	// grant its rotation retry — otherwise a single-image reply hitting
	// token-expiry is lost silently (#2305).
	for _, img := range msg.Images {
		id, err := f.sendImage(ctx, msg.ChatID, img)
		if err != nil {
			if invErr := f.maybeInvalidateOnTokenError(err); platform.IsTokenInvalidated(invErr) {
				return lastMsgID, invErr
			}
			slog.Warn("feishu send image failed", "err", err)
			continue
		}
		lastMsgID = id
	}

	return lastMsgID, nil
}

func (f *Feishu) sendText(ctx context.Context, chatID, text string) (string, error) {
	// Always a card so EditMessage (PATCH) can later replace it with markdown;
	// plain text cannot be edited into card format.
	return f.sendCard(ctx, chatID, text)
}

// Feishu interactive card, schema 2.0 (required for full GFM: headings,
// fenced code, tables, blockquotes). Typed so json.Marshal avoids
// map[string]any boxing on every reply.
type feishuMarkdownElement struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}
type feishuCardBody struct {
	Elements [1]feishuMarkdownElement `json:"elements"`
}
type feishuCard struct {
	Schema string         `json:"schema"`
	Body   feishuCardBody `json:"body"`
}

// buildMarkdownCardJSON marshals a single-markdown-element card.
func buildMarkdownCardJSON(text string) ([]byte, error) {
	card := feishuCard{
		Schema: "2.0",
		Body: feishuCardBody{
			Elements: [1]feishuMarkdownElement{{Tag: "markdown", Content: text}},
		},
	}
	// No HTML escaping: `<` etc. would render literally in the markdown element.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(card); err != nil {
		return nil, err
	}
	// Strip the Encoder's trailing '\n'; the outer Marshal expects a pure value.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// sendCard sends a Feishu interactive card with markdown content.
func (f *Feishu) sendCard(ctx context.Context, chatID, text string) (string, error) {
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("get access token: %w", err)
	}

	cardJSON, err := buildMarkdownCardJSON(text)
	if err != nil {
		return "", fmt.Errorf("marshal card: %w", err)
	}
	// `content` must be stringified JSON, not a nested object.
	reqBody, err := json.Marshal(struct {
		ReceiveID string `json:"receive_id"`
		MsgType   string `json:"msg_type"`
		Content   string `json:"content"`
	}{ReceiveID: chatID, MsgType: "interactive", Content: string(cardJSON)})
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIRespBodyBytes)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if result.Code != 0 {
		return "", &APIError{Code: result.Code, Msg: result.Msg, Op: "send"}
	}

	return result.Data.MessageID, nil
}

func (f *Feishu) replyError(_ context.Context, chatID, text string) {
	rctx, cancel := context.WithTimeout(f.stopCtx, 5*time.Second)
	defer cancel()
	if _, err := f.Reply(rctx, platform.OutgoingMessage{ChatID: chatID, Text: text}); err != nil {
		slog.Warn("feishu reply error failed", "err", err)
	}
}

// uploadImage uploads image data to Feishu and returns the image_key.
func (f *Feishu) EditMessage(ctx context.Context, msgID string, text string) error {
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	cardJSON, err := buildMarkdownCardJSON(text)
	if err != nil {
		return fmt.Errorf("marshal card: %w", err)
	}
	reqBody, err := json.Marshal(struct {
		Content string `json:"content"`
	}{Content: string(cardJSON)})
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}

	// PathEscape: a crafted ID with "/" or "?" must not redirect the PATCH.
	req, err := http.NewRequestWithContext(ctx, "PATCH",
		f.baseURL+"/open-apis/im/v1/messages/"+url.PathEscape(msgID),
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIRespBodyBytes)).Decode(&result); err != nil {
		return fmt.Errorf("decode edit response: %w", err)
	}
	if result.Code != 0 {
		// Structured so errors.As-based token refresh works on the edit path too.
		return &APIError{Code: result.Code, Msg: result.Msg, Op: "edit"}
	}
	return nil
}

// reactionRequestBody is the JSON body sent to POST /reactions (hot path:
// one call per dispatched IM message, typed to avoid map allocations).
