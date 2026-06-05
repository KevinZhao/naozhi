// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     subscriber block (clients / connCount / clientWG /
//	            wsAuthLimiter / wsUpgradeLimiter / upgrader / dashTokenHash /
//	            cookieMAC / trustedProxy) +
//	            rate-limit/cache block (connCountByOwnerMu / connCountByOwner)
//	READS:      shared deps block (dashToken / auth)
//
// Phase 4b 起 rule 3b 升级到 AST 字段访问对账时，会校验本文件方法体
// 的字段访问匹配本契约；当前 Phase 0b 仅 marker 存在性。
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
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

	// R20260527122801-SEC-2 / #1326: derive uploadOwner BEFORE
	// upgrader.Upgrade because Set-Cookie headers cannot be added once
	// the response is hijacked into a 101. In no-token mode without an
	// existing nz_anon cookie we must mint one here; mint failure refuses
	// the upgrade with 503 (matches HTTP uploadOwnerOrFail) so co-NAT
	// clients can never share an IP-derived uploadOwner bucket.
	ip := clientIP(r, h.trustedProxy)
	uploadOwnerKey, preAuthenticated, ok := wsDeriveUploadOwner(w, r, h, ip)
	if !ok {
		return
	}

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
	// R20260527122801-SEC-2 / #1326: uploadOwner + initial-authenticated
	// were resolved before Upgrade so any Set-Cookie minted there could
	// ride the 101 response. Apply the cached results here.
	c.setUploadOwner(uploadOwnerKey)
	if preAuthenticated {
		c.authenticated.Store(true)
	}
	// R229-SEC-8 / #1022: per-uploadOwner sub-cap. Reserve AFTER the
	// owner derivation above so the bucket is keyed by the same value
	// upload-quota / send-limiter use. The conn was already counted
	// against the global maxWSConns ceiling above, so a refusal here
	// must release that slot too: closing the upgraded conn lets the
	// outer slotReleased defer fire, and we bail before register().
	// Owner == "" (legacy single-user no-token path on first connect)
	// passes through unchanged.
	ownerSlotHeld := false
	if !h.reserveOwnerSlot(c.uploadOwnerKey()) {
		// Close the upgraded socket so the client sees a clean RST and
		// can reconnect after another tab disconnects. Do not write a
		// CloseFrame: the budget is exhausted at the boundary and we are
		// trying NOT to allocate the per-conn write buffer for it.
		conn.Close()
		return
	}
	ownerSlotHeld = true
	defer func() {
		if ownerSlotHeld && !slotReleased {
			h.releaseOwnerSlot(c.uploadOwnerKey())
		}
	}()
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

// wsDeriveUploadOwner runs the WS upgrade auth-cookie / nz_anon resolution
// BEFORE upgrader.Upgrade so any minted Set-Cookie header rides the 101
// response (after Upgrade the response is hijacked and headers can no
// longer be added).
//
// Returns:
//   - owner:           the uploadOwner key to assign to the new wsClient.
//   - authenticated:   whether the client should start in the authenticated
//     state (auth-cookie match in token mode, or no-token
//     mode where every connection is implicitly authed).
//   - ok:              false when the upgrade must be refused. The 503
//     reply has already been written; callers must
//     simply return.
//
// R20260527122801-SEC-2 (#1326): in no-token mode without an existing
// nz_anon cookie this helper mints one and refuses the upgrade if the
// system entropy source fails — mirroring HTTP uploadOwner's ok=false
// contract so co-NAT clients never share an IP-derived owner bucket.
// Test harnesses that wire NewHub without HubOptions.Auth retain the
// legacy IP-fallback (those Hubs do not run uploadStore so the fallback
// is not security-relevant).
func wsDeriveUploadOwner(w http.ResponseWriter, r *http.Request, h *Hub, ip string) (owner string, authenticated bool, ok bool) {
	if h.dashToken == "" {
		// No-token mode: every connection is authenticated; uploadOwner
		// derives from nz_anon (cookie or freshly minted).
		//
		// R236-SEC-06 (#485): only honour the inbound cookie when it
		// matches mintAnonCookie's wire shape (32 lowercase-hex chars).
		// An attacker on a shared NAT can set the nz_anon cookie to any
		// value via JS / a manual request, and ownerKeyFromCookie would
		// happily hash arbitrary bytes into a stable bucket. A malformed
		// value falls through to mint a fresh server-issued label so the
		// uploadOwner is always rooted in 16 bytes the server generated
		// and never in attacker-supplied content. Legitimately-minted
		// cookies pass the check unchanged so existing browser sessions
		// keep their bucket across the upgrade.
		if cookie, err := r.Cookie(anonCookieName); err == nil && isValidAnonCookieValue(cookie.Value) {
			return ownerKeyFromCookie(cookie.Value), true, true
		}
		if h.auth != nil {
			val, mintErr := mintAnonCookie(w, r, h.auth)
			if mintErr != nil {
				slog.Warn("ws upgrade: mintAnonCookie failed; refusing to fall back to IP-derived owner key",
					"err", mintErr, "remote", ip)
				w.Header().Set("Retry-After", "30")
				http.Error(w, "could not derive upload owner; please retry", http.StatusServiceUnavailable)
				return "", false, false
			}
			return ownerKeyFromCookie(val), true, true
		}
		// AuthHandlers not wired (test harness only). Preserve the
		// legacy IP-fallback so unit tests that bypass NewHub continue
		// to authenticate.
		return ip, true, true
	}
	// Token mode: only the auth-cookie path is examined here. WS clients
	// that present `Authorization: Bearer ...` (browser-side EventSource
	// shim) authenticate via the inner handleAuth message, where token-
	// hash → uploadOwner derivation runs. Until then the client stays
	// unauthenticated with empty uploadOwner — that branch never reaches
	// uploadStore.
	if cookie, err := r.Cookie(auth.AuthCookieName); err == nil {
		// R040034-SEC-1 (#1398): h.cookieMAC is a getter callback so
		// RotateCookieGen invalidations reach this branch on the next
		// upgrade — previously the Hub cached opts.CookieMAC at
		// construction, leaving WS upgrades accepting pre-rotation
		// cookies until the process restarted. Local var so the
		// constant-time compare and the empty-guard read the same value.
		mac := h.cookieMAC()
		if mac != "" && subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(mac)) == 1 {
			// Must use the same derivation as HTTP uploadOwner so files
			// uploaded on one transport can be claimed on the other.
			return ownerKeyFromCookie(cookie.Value), true, true
		}
	}
	return "", false, true
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
		serverMetrics.WSAuthFail()
		serverMetrics.WSAuthFailRateLimited()
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
		// Derive uploadOwner from the provided token so WS token-auth enforces
		// the same per-owner upload quota as HTTP Bearer auth. Without this,
		// a WS-token-authed client could bypass maxUploadPerOwner because
		// uploadOwner stayed "" (empty string matches every "" owner in the
		// store). Mirrors the derivation in dashboard_send.go uploadOwner().
		// R67-SEC-1.
		//
		// #1775: the per-owner conn slot was reserved at upgrade time against
		// the pre-auth owner ("" in token mode, a guaranteed no-op). Re-key the
		// reservation BEFORE flipping authenticated so reserve/release stay
		// paired against the SAME owner — otherwise unregister releases a slot
		// under newOwner that was never reserved, corrupting the per-owner
		// counter and (over reconnects) wedging the maxConnsPerOwner cap with
		// phantom slots. release(old="") then reserve(new) re-enters the cap
		// under the real owner. A reserve failure means newOwner is already at
		// the per-owner ceiling: refuse the auth (leave the conn unauthenticated
		// with its original owner) rather than admitting a connection whose slot
		// we could not account for. Doing this before authenticated.Store keeps
		// the reject path from leaving a half-authenticated client behind.
		oldOwner := c.uploadOwnerKey()
		if oldOwner == "" && msg.Token != "" {
			// R247-SEC-16: 128-bit owner key, parity with HTTP path
			// (dashboard_send.go ownerKeyFromCookie / Bearer hash).
			sum := sha256.Sum256([]byte(msg.Token))
			newOwner := hex.EncodeToString(sum[:16])
			h.releaseOwnerSlot(oldOwner)
			if !h.reserveOwnerSlot(newOwner) {
				// Re-claim the old slot so teardown's releaseOwnerSlot(oldOwner)
				// stays balanced, then refuse. Owner stays oldOwner; closing the
				// conn unwinds the pumps which unregister against oldOwner.
				h.reserveOwnerSlot(oldOwner)
				c.SendRaw([]byte(wsAuthFailInvalidMsg))
				serverMetrics.WSAuthFail()
				if c.conn != nil {
					_ = c.conn.Close()
				}
				return
			}
			c.setUploadOwner(newOwner)
		}
		c.authenticated.Store(true)
		// R040034-PERF-23 (#1409): mirror the auth flip into h.authClients
		// so broadcastToAuthenticated can iterate the mirror directly
		// instead of filtering every connected client × per-element
		// authenticated.Load(). The Store above must precede the mirror
		// write so a concurrent broadcast that observes the mirror entry
		// also observes a true authenticated flag (defence-in-depth: the
		// broadcast no longer reads the flag, but other call sites
		// covering wsclient.go still do).
		h.markAuthenticated(c)
		c.SendRaw([]byte(wsAuthOkMsg))
	} else {
		c.SendRaw([]byte(wsAuthFailInvalidMsg))
		// R172-ARCH-D10: also bump the dedicated "invalid-token" split so
		// operators can distinguish credential spray (this counter rising)
		// from throttling storms (*RateLimitedTotal rising).
		serverMetrics.WSAuthFail()
		serverMetrics.WSAuthFailInvalidToken()
	}
}
