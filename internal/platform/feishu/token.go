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

// Tenant-access-token lifecycle: singleflight-merged refresh, circuit breaker,
// and early-revocation invalidation. Token state lives on *Feishu.

// invalidateAccessToken clears the cached token so the next getAccessToken
// refreshes: Feishu can revoke early (rotation, admin action) and the cache
// would otherwise hold the stale value until its TTL. Also clears the circuit
// breaker — a structured error proves the remote is responsive.
func (f *Feishu) invalidateAccessToken() {
	f.tokenMu.Lock()
	f.accessToken = ""
	f.tokenExpiry = time.Time{}
	f.tokenLastFailed = nil
	f.tokenLastFailAt = time.Time{}
	f.tokenMu.Unlock()
}

// maybeInvalidateOnTokenError calls invalidateAccessToken when err carries a
// token-expired APIError and returns err unchanged; every outbound API path
// uses it as a post-call hook.
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
// The caller's ctx is intentionally ignored: singleflight merges concurrent
// refreshes into one HTTP call, and one caller's cancellation would fail all
// of them. The refresh is bounded by f.stopCtx + 10s instead.
func (f *Feishu) getAccessToken(_ context.Context) (string, error) {
	// One RLock covers both the freshness and circuit-breaker reads so a
	// concurrent refresh cannot mutate state between them.
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

	v, err, _ := f.tokenGroup.Do("token", func() (any, error) {
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

		// Defer ORDER contract: `defer cancel()` here, `defer resp.Body.Close()`
		// below, so Body.Close runs before cancel (LIFO). Reversing them would
		// abort the Decode read mid-flight. Do not insert defers between them.
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
		// Expire is normally ≈7200 but can be 0/tiny; without the floor the
		// buffered TTL goes negative and every call refreshes.
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
		// Clear the circuit breaker on success.
		f.tokenLastFailed = nil
		f.tokenLastFailAt = time.Time{}
		f.tokenMu.Unlock()

		return result.TenantAccessToken, nil
	})
	if err != nil {
		// Arm the circuit breaker for tokenFailCooldown.
		f.tokenMu.Lock()
		f.tokenLastFailed = err
		f.tokenLastFailAt = time.Now()
		f.tokenMu.Unlock()
		return "", err
	}
	token, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("unexpected token type %T", v)
	}
	return token, nil
}
