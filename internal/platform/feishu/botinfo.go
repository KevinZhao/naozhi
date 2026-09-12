package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// botinfo.go — the bot's own identity: fetching it, refreshing it under a
// cooldown, and deciding whether an inbound message mentioned us. Extracted from
// feishu.go (J10 of #2548).

func (f *Feishu) fetchBotInfo(ctx context.Context) error {
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", f.baseURL+"/open-apis/bot/v3/info", nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := feishuHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			OpenID  string `json:"open_id"`
			AppName string `json:"app_name"`
		} `json:"bot"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIRespBodyBytes)).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if result.Code != 0 {
		return &APIError{Code: result.Code, Msg: result.Msg, Op: "bot_info"}
	}
	if result.Bot.OpenID == "" {
		return fmt.Errorf("bot_info: empty open_id in response")
	}

	f.botInfoMu.Lock()
	f.botOpenID = result.Bot.OpenID
	f.botInfoMu.Unlock()
	// Upstream body could carry C1/bidi/newline bytes under a TLS MITM.
	slog.Info("feishu bot identity",
		"open_id", osutil.SanitizeForLog(result.Bot.OpenID, 64),
		"app_name", osutil.SanitizeForLog(result.Bot.AppName, 128))
	return nil
}

// isBotMentioned reports whether any mention targets this bot; when botOpenID
// is unknown any mention counts, so a degraded Start does not drop responses.
// The extractor closure lets the webhook and WebSocket schemas share the logic.
func (f *Feishu) isBotMentioned(count int, openIDAt func(i int) string) bool {
	f.botInfoMu.RLock()
	botID := f.botOpenID
	f.botInfoMu.RUnlock()
	if botID == "" {
		// Degraded: kick a rate-limited re-fetch so the open_id self-heals and
		// the next group mention matches strictly (#1009).
		if count > 0 {
			f.maybeRefreshBotInfo()
		}
		return count > 0
	}
	for i := 0; i < count; i++ {
		if openIDAt(i) == botID {
			return true
		}
	}
	return false
}

// botInfoRefreshCooldown rate-limits the self-heal re-fetch: short enough to
// recover from a transient Start failure, long enough that a revoked app does
// not cost a per-message API call.
const botInfoRefreshCooldown = time.Minute

// maybeRefreshBotInfo kicks a rate-limited, singleflight-merged background
// re-fetch of the bot's open_id while isBotMentioned is degraded. Non-blocking;
// tracked on f.dispatch so Stop() waits for it.
func (f *Feishu) maybeRefreshBotInfo() {
	// nil only for directly-constructed test fixtures; no lifecycle to anchor to.
	if f.stopCtx == nil {
		return
	}
	now := time.Now().UnixNano()
	last := atomic.LoadInt64(&f.lastBotInfoFetchNs)
	// delta < 0 = NTP step backwards: re-anchor rather than wedge the cooldown.
	if delta := now - last; delta >= 0 && delta < int64(botInfoRefreshCooldown) {
		return
	}
	if !atomic.CompareAndSwapInt64(&f.lastBotInfoFetchNs, last, now) {
		return
	}
	// Stop() cancels stopCtx then waits on dispatch; an Add after the counter
	// drained would panic, so re-check cancellation after the CAS and before Go.
	if f.stopCtx.Err() != nil {
		return
	}
	f.dispatch.Go("feishu bot info refresh", func() {
		// Constant key: one bot identity per adapter.
		_, _, _ = f.botInfoSF.Do("bot_info", func() (any, error) {
			ctx, cancel := context.WithTimeout(f.stopCtx, 5*time.Second)
			defer cancel()
			if err := f.fetchBotInfo(ctx); err != nil {
				slog.Warn("feishu lazy bot info re-fetch failed — group mention filtering stays in 'any @' fallback",
					"err", err)
				return nil, err
			}
			slog.Info("feishu bot open_id self-healed via lazy re-fetch — group mentions now matched strictly")
			return nil, nil
		})
	})
}

// Stop implements RunnablePlatform. Stops WebSocket connection.
