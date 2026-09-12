package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

// reactions.go — the Reactor capability: adding and removing a reaction on an
// inbound message. Extracted from feishu.go (J10 of #2548).

type reactionRequestBody struct {
	ReactionType reactionTypeField `json:"reaction_type"`
}

type reactionTypeField struct {
	EmojiType string `json:"emoji_type"`
}

// AddReaction implements platform.Reactor: creates the reaction and caches the
// returned reaction_id so RemoveReaction can delete by id.
func (f *Feishu) AddReaction(ctx context.Context, messageID string, r platform.ReactionType) error {
	if messageID == "" {
		return fmt.Errorf("feishu AddReaction: empty messageID")
	}
	emojiType := reactionEmojiType(r)
	if emojiType == "" {
		return fmt.Errorf("feishu AddReaction: unsupported reaction %q", r)
	}
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}
	reqBody, err := json.Marshal(reactionRequestBody{
		ReactionType: reactionTypeField{EmojiType: emojiType},
	})
	if err != nil {
		return fmt.Errorf("marshal reaction request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		f.baseURL+"/open-apis/im/v1/messages/"+url.PathEscape(messageID)+"/reactions",
		bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create reaction request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := feishuHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("post reaction: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int `json:"code"`
		Data struct {
			ReactionID string `json:"reaction_id"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIRespBodyBytes)).Decode(&result); err != nil {
		return fmt.Errorf("decode reaction response: %w", err)
	}
	if result.Code != 0 {
		// %q escapes bidi/C1/newline in the upstream msg.
		return fmt.Errorf("feishu reaction api: code=%d msg=%q", result.Code, result.Msg)
	}
	if result.Data.ReactionID != "" {
		// Expiry lets cleanupNoncesTick GC entries that never see RemoveReaction.
		f.reactionIDs.Store(reactionCacheKey(messageID, emojiType), reactionCacheEntry{
			id:     result.Data.ReactionID,
			expiry: time.Now().Add(reactionCacheTTL).UnixNano(),
		})
	}
	return nil
}

// RemoveReaction implements platform.Reactor via the cached reaction_id; with
// no cached id (restart between Add and Remove) it returns nil and the
// reaction lingers — acceptable for best-effort UX feedback.
func (f *Feishu) RemoveReaction(ctx context.Context, messageID string, r platform.ReactionType) error {
	if messageID == "" {
		return nil
	}
	emojiType := reactionEmojiType(r)
	if emojiType == "" {
		return nil
	}
	cacheKey := reactionCacheKey(messageID, emojiType)
	// Load, not LoadAndDelete: evict only AFTER the DELETE is confirmed so a
	// transient failure keeps the id for retry and ⏳ can still be cleared (#1984).
	v, ok := f.reactionIDs.Load(cacheKey)
	if !ok {
		return nil
	}
	entry, ok := v.(reactionCacheEntry)
	if !ok || entry.id == "" {
		// A malformed entry can never produce a valid DELETE; drop it.
		f.reactionIDs.Delete(cacheKey)
		return nil
	}
	reactionID := entry.id
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "DELETE",
		f.baseURL+"/open-apis/im/v1/messages/"+url.PathEscape(messageID)+"/reactions/"+url.PathEscape(reactionID),
		nil)
	if err != nil {
		return fmt.Errorf("create delete reaction request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := feishuHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete reaction: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIRespBodyBytes)).Decode(&result); err != nil {
		return fmt.Errorf("decode delete reaction response: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("feishu delete reaction api: code=%d msg=%q", result.Code, result.Msg)
	}
	f.reactionIDs.Delete(cacheKey)
	return nil
}
