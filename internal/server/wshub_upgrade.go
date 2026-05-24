package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/node"
)

// File: wshub_upgrade.go
//
// HTTP→WebSocket upgrade and the inner-message auth handshake extracted from
// wshub.go (R243-ARCH-2 split). Owns:
//   - wsAuthRetryAfterSeconds: advisory header in WS auth_fail rate-limit
//     replies (mirrors HTTP /api/auth/login Retry-After)
//   - HandleUpgrade: HTTP entry point + IP rate limit + dashboard-token
//     pre-check + websocket.Upgrader.Upgrade + clientWG-tracked pump spawn
//   - handleAuth: WS "auth" message handler (constant-time token compare,
//     rate-limited reply, log-injection-safe error path)
//
// All Hub state used by these helpers stays on *Hub. Pure code-relocation.

// wsAuthRetryAfterSeconds is the advisory "try again in N seconds" value the
// WS auth_fail rate-limit reply carries. It intentionally mirrors the
// HTTP /api/auth/login Retry-After header (60s) so the front-end can share
// one countdown helper across HTTP login and WS auth paths. The underlying
// limiter refills a token every 12s (burst=5); 60s is a conservative upper
// bound that avoids sending users into another back-to-back 429 loop.
const wsAuthRetryAfterSeconds = 60

func (h *Hub) HandleUpgrade(w http.ResponseWriter, r *http.Request) {
	// R191-SEC-M2: Per-IP rate limit at the upgrade boundary uses the
	// *separate* wsUpgradeLimiter bucket (20/s burst, 60/min sustained) so
	// legitimate tab-reload / mobile-wake bursts do not consume the tight
	// loginLimiter budget used by password brute-force defence. The inner
	// `auth` WS message (handleAuth) continues to call wsAuthLimiter which
	// draws from loginLimiter (5/min burst) — direct credential tests keep
	// the strict budget. Fallback to wsAuthLimiter preserves behaviour for
	// tests that only wire the old field.
	limiterFn := h.wsUpgradeLimiter
	if limiterFn == nil {
		limiterFn = h.wsAuthLimiter
	}
	if limiterFn != nil {
		// The underlying *Allow implementations map "" to a shared
		// unknown-IP bucket, so we do not skip the check on empty IP —
		// that would let malformed RemoteAddr bypass the per-IP budget
		// entirely.
		if !limiterFn(clientIP(r, h.trustedProxy)) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
	}
	// Reject upgrades when too many connections are open (prevent resource exhaustion
	// from unauthenticated connections allocating goroutines + channel buffers).
	// Reserve the slot atomically: the previous RLock/check/unlock sequence was a
	// TOCTOU window where a concurrent burst could all observe count < cap and
	// all complete the upgrade. CAS on connCount collapses the gate into one step.
	if n := h.connCount.Add(1); n > maxWSConns {
		h.connCount.Add(-1)
		http.Error(w, "too many WebSocket connections", http.StatusServiceUnavailable)
		return
	}
	// Release the reserved slot on any pre-register failure path.
	slotReleased := false
	defer func() {
		if !slotReleased {
			h.connCount.Add(-1)
		}
	}()

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Capture origin + remote IP so operators can diagnose
		// CheckOrigin rejections or attribute floods to a specific client
		// without digging through raw request logs.
		slog.Debug("ws upgrade failed",
			"err", err,
			"remote", clientIP(r, h.trustedProxy),
			"origin", r.Header.Get("Origin"),
			"host", r.Host)
		return
	}
	// Read-limit is owned by readPump (wsMaxMessageSize). Previous code also
	// set it here with a different value, which masked the real cap since
	// readPump re-applies wsMaxMessageSize on first iteration — remove the
	// redundant setter to keep a single source of truth.
	ip := clientIP(r, h.trustedProxy)
	c := &wsClient{
		conn: conn,
		// Buffer holds outbound event frames (CLI output, subscription
		// updates). 256 is sized for brief latency spikes so slow consumers
		// drop rather than balloon memory. Outbound "history" batches are
		// capped at maxHistoryPushEntries in eventPushLoop (≤~50 × ~200 B =
		// ~10 KB per frame), so 256 slots × ~10 KB = ~2.5 MB worst-case
		// per-client. R68-PERF-H1.
		send:        make(chan []byte, 256),
		hub:         h,
		remoteIP:    ip,
		sendLimiter: rate.NewLimiter(rate.Every(time.Second), 5), // 5 sends/s burst, 1/s sustained
		// Interrupt budget intentionally tighter than send: a human pressing
		// "stop" never needs more than once per second, but an attacker who
		// can spam interrupts can DoS a session by aborting every turn it
		// starts. ~0.5/s sustained, burst 2 covers double-clicks. Was 5/s
		// burst 3 (more permissive than sends), which let auth'd clients
		// override the send budget on the interrupt path.
		interruptLimiter: rate.NewLimiter(rate.Every(2*time.Second), 2),
		subscriptions:    make(map[string]func()),
		subGen:           make(map[string]uint64),
		done:             make(chan struct{}),
	}
	if h.dashToken == "" {
		c.authenticated.Store(true)
		// R233-SEC-10: prefer the per-browser nz_anon cookie over raw
		// client IP so co-NAT users don't share an uploadOwner bucket and
		// claim each other's TakeAll uploads. The HTTP path mints the
		// cookie via uploadOwner→mintAnonCookie before any WS upgrade
		// (dashboard JS hits /api/health → triggers uploadOwner derive in
		// that flow); when the cookie is absent we fall back to client IP
		// to preserve the prior contract — IP-fallback only loses
		// disambiguation between co-NAT clients that have never made an
		// HTTP request first, which is rare in practice.
		if cookie, err := r.Cookie(anonCookieName); err == nil && cookie.Value != "" {
			c.uploadOwner = ownerKeyFromCookie(cookie.Value)
		} else {
			c.uploadOwner = ip
		}
	} else if cookie, err := r.Cookie(authCookieName); err == nil {
		if h.cookieMAC != "" && subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(h.cookieMAC)) == 1 {
			c.authenticated.Store(true)
			// Must use the same derivation as HTTP uploadOwner so files
			// uploaded on one transport can be claimed on the other.
			c.uploadOwner = ownerKeyFromCookie(cookie.Value)
		}
	}
	// Arm clientWG BEFORE registering the client, not after. If Shutdown
	// runs between register() and Add(2), it could snapshot h.clients,
	// close the conn, observe clientWG count == 0, and return before the
	// pumps ever increment — leaving them to run past teardown and
	// use-after-free router/hub state. Add is cheap and always balanced
	// by the deferred Done() in the pump goroutines below.
	h.clientWG.Add(2)
	h.register(c)
	// Ownership of the connCount slot transfers to register/unregister:
	// mark the slot as released here so the defer on the upgrade path
	// doesn't double-decrement. unregister() will Add(-1) when this
	// client eventually disconnects.
	slotReleased = true
	go func() { defer h.clientWG.Done(); c.writePump() }()
	go func() { defer h.clientWG.Done(); c.readPump() }()
}


func (h *Hub) handleAuth(c *wsClient, msg node.ClientMsg) {
	// Per-IP rate limit to prevent brute-force via rapid connect/auth/disconnect cycles.
	if h.wsAuthLimiter != nil && !h.wsAuthLimiter(c.remoteIP) {
		// Advisory RetryAfter matches the HTTP /api/auth/login 429 branch
		// (dashboard_auth.go writes Retry-After: 60) so WS and HTTP auth
		// lockouts surface identical countdowns on the front end. Clients
		// older than R110-P2 ignore the field; new clients visually gate
		// re-auth until the window elapses.
		c.SendJSON(node.ServerMsg{
			Type:       "auth_fail",
			Error:      "too many attempts",
			RetryAfter: wsAuthRetryAfterSeconds,
		})
		// OBS2: rate-limit-triggered auth_fail counts for brute-force detection
		// the same way invalid-token auth_fail does — operators watching
		// naozhi_ws_auth_fail_total should see both signals blended, and
		// Retry-After tells them whether the limiter is actively engaging.
		// R172-ARCH-D10: also bump the dedicated "rate-limited" split so
		// operators can tell whether the limiter is the dominant source of
		// auth_fail (e.g. looping client) vs a credential spray pacing under
		// the limiter threshold.
		metrics.WSAuthFailTotal.Add(1)
		metrics.WSAuthFailRateLimitedTotal.Add(1)
		return
	}
	// Short-circuit when the connection is already authenticated via cookie —
	// do not touch msg.Token or run the ConstantTimeCompare so the
	// cookie-authed and token-authed paths are cleanly separated.
	if c.authenticated.Load() {
		c.SendRaw([]byte(wsAuthOkMsg))
		return
	}
	// Pre-hash both sides to normalize length — subtle.ConstantTimeCompare
	// returns 0 immediately when operand lengths differ, leaking the token
	// length via response latency. HTTP Bearer path (dashboard_auth.go:113)
	// already applies this pattern; mirror it here so brute-force attackers
	// cannot discover the correct token length via the WS auth endpoint.
	tokenOK := false
	if h.dashToken != "" {
		got := sha256.Sum256([]byte(msg.Token))
		// R230-PERF-11: h.dashTokenHash is precomputed at Hub construction;
		// we only hash the inbound token per auth attempt.
		tokenOK = subtle.ConstantTimeCompare(got[:], h.dashTokenHash[:]) == 1
	}
	if h.dashToken == "" || tokenOK {
		// R228-GO-P3-3: distinguish "no token configured (single-user
		// trust)" from "valid token presented" so operators can tell from
		// logs which mode their deployment is running in.
		if h.dashToken == "" {
			slog.Debug("ws auth: no-token mode, authenticating unconditionally")
		}
		c.authenticated.Store(true)
		// Derive uploadOwner from the provided token so WS token-auth enforces
		// the same per-owner upload quota as HTTP Bearer auth. Without this,
		// a WS-token-authed client could bypass maxUploadPerOwner because
		// uploadOwner stayed "" (empty string matches every "" owner in the
		// store). Mirrors the derivation in dashboard_send.go uploadOwner().
		// R67-SEC-1.
		if c.uploadOwner == "" && msg.Token != "" {
			sum := sha256.Sum256([]byte(msg.Token))
			c.uploadOwner = hex.EncodeToString(sum[:8])
		}
		c.SendRaw([]byte(wsAuthOkMsg))
	} else {
		c.SendRaw([]byte(wsAuthFailInvalidMsg))
		// R172-ARCH-D10: also bump the dedicated "invalid-token" split so
		// operators can distinguish credential spray (this counter rising)
		// from throttling storms (*RateLimitedTotal rising).
		metrics.WSAuthFailTotal.Add(1)
		metrics.WSAuthFailInvalidTokenTotal.Add(1)
	}
}
