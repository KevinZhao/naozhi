package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// File: token.go
//
// Tenant-access-token lifecycle relocated from feishu.go (R20260607-ARCH-3 /
// #1906): getAccessToken (singleflight-merged refresh + circuit breaker),
// invalidateAccessToken (early-revocation cache clear), and
// maybeInvalidateOnTokenError (post-call hook). All token state lives on
// *Feishu (tokenMu / accessToken / tokenExpiry / tokenLastFailed /
// tokenLastFailAt / tokenGroup) and the related constants stay in feishu.go.
// Pure code-relocation — no behaviour change.

// invalidateAccessToken clears the cached tenant access token so the next
// getAccessToken call forces a refresh. Called when an API response reports
// "tenant access token invalid" (APIError.IsTokenExpired) — the cache's
// `tokenExpiry` guard normally keeps a token until ~60s before its stated
// TTL, but Feishu can revoke a token early (credential rotation, admin
// action) and in that case the cache holds a stale value until the 2-hour
// TTL naturally lapses. Without this invalidation ReplyWithRetry's 3
// attempts all present the same rejected token and all fail identically.
//
// Also clears tokenLastFailed so a preceding circuit-breaker state does
// not refuse the fresh attempt: the caller has just received confirmed
// evidence that the remote is responsive (it returned a structured error,
// not a network failure), so the preceding failure cooldown is irrelevant.
// R83 / RETRY1.
func (f *Feishu) invalidateAccessToken() {
	f.tokenMu.Lock()
	f.accessToken = ""
	f.tokenExpiry = time.Time{}
	f.tokenLastFailed = nil
	f.tokenLastFailAt = time.Time{}
	f.tokenMu.Unlock()
}

// maybeInvalidateOnTokenError calls invalidateAccessToken if err's chain
// carries an APIError with IsTokenExpired() == true. Returns err unchanged.
// Reply/sendText/sendCard/sendImage use this as a post-call hook so the
// next ReplyWithRetry attempt will see a fresh token. Kept as a helper
// rather than inline at every call site so future outbound API additions
// (edit card, delete message, upload file) cannot accidentally omit the
// cache invalidation when they return APIErrors.
func (f *Feishu) maybeInvalidateOnTokenError(err error) error {
	if err == nil {
		return nil
	}
	var api *APIError
	if errors.As(err, &api) && api.IsTokenExpired() {
		slog.Warn("feishu tenant access token rejected, invalidating cache",
			"code", api.Code, "op", api.Op)
		f.invalidateAccessToken()
	}
	return err
}

// getAccessToken returns a valid tenant access token, refreshing if needed.
// Uses singleflight to merge concurrent refresh requests.
//
// The caller's ctx is intentionally ignored for the refresh request: when
// many request goroutines collide on an expired token, singleflight merges
// them into one HTTP call; honouring any single caller's cancellation would
// abort the shared refresh and fail every merged caller. Instead we bound
// the refresh with f.stopCtx + 10s so Stop() still aborts it promptly.
func (f *Feishu) getAccessToken(_ context.Context) (string, error) {
	// Fast path: single RLock checks both cached-token freshness and the
	// circuit-breaker. Splitting these into two separate RLock blocks used
	// to create a window where a concurrent refresh could mutate
	// tokenLastFailed between the two reads, letting a stale token be
	// returned or the circuit breaker be bypassed.
	f.tokenMu.RLock()
	if f.accessToken != "" && time.Now().Before(f.tokenExpiry) {
		token := f.accessToken
		f.tokenMu.RUnlock()
		return token, nil
	}
	if f.tokenLastFailed != nil && time.Since(f.tokenLastFailAt) < tokenFailCooldown {
		err := f.tokenLastFailed
		f.tokenMu.RUnlock()
		return "", err
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

		// Derive the refresh context from the long-lived stopCtx rather than
		// the caller's ctx so one caller's cancellation does not torpedo the
		// singleflight-merged refresh for all concurrent callers. Stop() still
		// aborts by cancelling stopCtx. singleflight shares the returned
		// (v, err) value with late callers — they never see refreshCtx — so
		// the `defer cancel()` here only bounds the in-flight HTTP request.
		//
		// R182-GO-P1-3 LIFO ORDERING CONTRACT: `defer cancel()` is registered
		// here, `defer resp.Body.Close()` is registered below after Do()
		// returns. Go runs defers in reverse order, so Body.Close() runs
		// before cancel() — which is required: cancel() before Body.Close()
		// would mid-abort the Decode's LimitReader read in the error-path
		// re-try case. DO NOT insert additional defers between these two or
		// convert either to an explicit call without preserving this order.
		refreshCtx, cancel := context.WithTimeout(f.stopCtx, 10*time.Second)
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
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIRespBodyBytes)).Decode(&result); err != nil {
			return nil, fmt.Errorf("decode token response: %w", err)
		}
		if result.Code != 0 {
			return nil, &APIError{Code: result.Code, Op: "token"}
		}
		// Feishu normally returns Expire≈7200, but edge cases (clock skew,
		// API misbehaviour) can yield 0 or very small values. Without the
		// clamp `result.Expire-60` underflows to a negative number, so
		// `time.Now().Add(...)` produces an already-expired deadline and every
		// subsequent call would fire a fresh refresh. Treat anything below 60s
		// as "honour the 30s minimum caching window" to keep singleflight effective.
		if result.TenantAccessToken == "" {
			return nil, &APIError{Code: result.Code, Msg: "empty token", Op: "token"}
		}
		ttl := time.Duration(result.Expire-tokenTTLBuffer) * time.Second
		if ttl < minTokenCacheDuration {
			ttl = minTokenCacheDuration
		}

		f.tokenMu.Lock()
		f.accessToken = result.TenantAccessToken
		f.tokenExpiry = time.Now().Add(ttl)
		// Clear circuit breaker on success so a transient failure does not
		// keep blocking future refreshes after recovery.
		f.tokenLastFailed = nil
		f.tokenLastFailAt = time.Time{}
		f.tokenMu.Unlock()

		return result.TenantAccessToken, nil
	})
	if err != nil {
		// Record the failure so subsequent callers within the cooldown
		// short-circuit without another HTTP round-trip.
		f.tokenMu.Lock()
		f.tokenLastFailed = err
		f.tokenLastFailAt = time.Now()
		f.tokenMu.Unlock()
		return "", err
	}
	// Defensive type assertion: singleflight.Do returns `any` and the callback
	// path already returns a string, but guard against accidental refactor
	// regression (e.g., returning a struct wrapper) rather than panicking
	// on hot auth path.
	token, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("unexpected token type %T", v)
	}
	return token, nil
}
