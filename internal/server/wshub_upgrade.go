// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     subscriber block (clients / connCount / clientWG /
//	            wsAuthLimiter / wsUpgradeLimiter / upgrader / dashTokenHash /
//	            cookieMAC / trustedProxy) +
//	            rate-limit/cache block (connCountByOwnerMu / connCountByOwner)
//	READS:      shared deps block (dashToken / auth)
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/wsproto"
)

// wsAuthRetryAfterSeconds is the advisory "try again in N seconds" value in
// the WS auth_fail rate-limit reply. It mirrors the HTTP /api/auth/login
// Retry-After (60s) so the front-end shares one countdown helper; the limiter
// refills a token every 12s (burst=5), so 60s avoids a back-to-back 429 loop.
const wsAuthRetryAfterSeconds = 60

func (h *Hub) HandleUpgrade(w http.ResponseWriter, r *http.Request) {
	// Per-IP limit at the upgrade boundary uses the separate wsUpgradeLimiter
	// bucket so tab-reload / mobile-wake bursts do not consume the tight
	// loginLimiter budget; the inner `auth` message (handleAuth) still draws
	// from the strict wsAuthLimiter. The fallback keeps tests that wire only
	// the old field working.
	limiterFn := h.wsUpgradeLimiter
	if limiterFn == nil {
		limiterFn = h.wsAuthLimiter
	}
	if limiterFn != nil {
		// Trusted-proxy mode: an XFF-less request collapses to "" → the shared
		// unknownIPKey bucket, letting one attacker starve the budget for every
		// other XFF-less caller. Fail closed, like HandleLogin (#2120).
		if !requestHasResolvableClientIP(r, h.trustedProxy) {
			errRespRetry(w, http.StatusTooManyRequests, "rate_limited", "too many requests", wsAuthRetryAfterSeconds)
			return
		}
		// *Allow maps "" to a shared unknown-IP bucket; do not skip the check on
		// empty IP or a malformed RemoteAddr would bypass the per-IP budget.
		if !limiterFn(clientIP(r, h.trustedProxy)) {
			errRespRetry(w, http.StatusTooManyRequests, "rate_limited", "too many requests", wsAuthRetryAfterSeconds)
			return
		}
	}
	// Reserve a connection slot atomically (CAS on connCount) so a concurrent
	// burst cannot all observe count < cap and complete the upgrade.
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

	// Derive uploadOwner BEFORE upgrader.Upgrade: Set-Cookie cannot be added
	// once the response is hijacked into a 101. Mint failure refuses the
	// upgrade (503) so co-NAT clients never share an IP-derived owner (#1326).
	ip := clientIP(r, h.trustedProxy)
	uploadOwnerKey, preAuthenticated, ok := wsDeriveUploadOwner(w, r, h, ip)
	if !ok {
		return
	}

	// gorilla's Upgrade writes the 101 itself; headers set on w (mintAnonCookie /
	// renewAnonCookie) are DROPPED unless forwarded via responseHeader.
	var respHdr http.Header
	if sc := w.Header().Values("Set-Cookie"); len(sc) > 0 {
		respHdr = http.Header{"Set-Cookie": sc}
	}
	conn, err := h.upgrader.Upgrade(w, r, respHdr)
	if err != nil {
		// Origin + remote IP let operators diagnose CheckOrigin rejections.
		slog.Debug("ws upgrade failed",
			"err", err,
			"remote", clientIP(r, h.trustedProxy),
			"origin", r.Header.Get("Origin"),
			"host", r.Host)
		return
	}
	// Read-limit is owned by readPump (wsMaxMessageSize); do not set it here.
	c := &wsClient{
		conn: conn,
		// Outbound frames; 256 slots absorb brief latency spikes so slow
		// consumers drop rather than balloon memory. History pushes are capped at
		// maxHistoryPushEntries (~10 KB/frame) → ~2.5 MB worst case per client.
		send:        make(chan []byte, 256),
		hub:         h,
		remoteIP:    ip,
		sendLimiter: rate.NewLimiter(rate.Every(time.Second), 5), // 5 sends/s burst, 1/s sustained
		// Interrupt budget is tighter than send: a human never needs "stop" more
		// than ~once per second, while spammed interrupts could abort every turn.
		// ~0.5/s sustained, burst 2 covers double-clicks.
		interruptLimiter: rate.NewLimiter(rate.Every(2*time.Second), 2),
		subscriptions:    make(map[string]func()),
		subGen:           make(map[string]uint64),
		done:             make(chan struct{}),
	}
	// Apply the owner / initial-auth results resolved before Upgrade (#1326).
	c.setUploadOwner(uploadOwnerKey)
	if preAuthenticated {
		c.authenticated.Store(true)
	}
	// Per-uploadOwner sub-cap, keyed by the same owner value upload-quota /
	// send-limiter use (#1022). The conn already counts against maxWSConns, so
	// a refusal must release that slot too: closing the conn lets the
	// slotReleased defer fire, and we bail before register(). Owner == ""
	// (legacy single-user no-token path) passes through unchanged.
	ownerSlotHeld := false
	if !h.reserveOwnerSlot(c.uploadOwnerKey()) {
		// Close so the client sees a clean RST; no CloseFrame, to avoid allocating
		// the per-conn write buffer at an exhausted boundary.
		conn.Close()
		return
	}
	ownerSlotHeld = true
	defer func() {
		if ownerSlotHeld && !slotReleased {
			h.releaseOwnerSlot(c.uploadOwnerKey())
		}
	}()
	// Arm clientWG BEFORE register(): if Shutdown ran between register() and
	// Add(2) it could observe count == 0 and return before the pumps start,
	// leaving them to run past teardown on torn-down router/hub state.
	h.clientWG.Add(2)
	h.register(c)
	// Slot ownership transfers to register/unregister; unregister() Add(-1)s
	// on disconnect, so the upgrade-path defer must not double-decrement.
	slotReleased = true
	go func() { defer h.clientWG.Done(); c.writePump() }()
	go func() { defer h.clientWG.Done(); c.readPump() }()
}

// wsDeriveUploadOwner runs the WS upgrade auth-cookie / nz_anon resolution
// BEFORE upgrader.Upgrade so any minted Set-Cookie rides the 101 response.
//
// Returns the uploadOwner key for the new wsClient, whether the client starts
// authenticated (auth-cookie match in token mode, or no-token mode), and
// ok=false when the upgrade must be refused (the 503 reply has already been
// written; callers simply return). In no-token mode without a valid nz_anon
// cookie it mints one and refuses if the entropy source fails, so co-NAT
// clients never share an IP-derived owner bucket (#1326). Hubs built without
// HubOptions.Auth (test harnesses) keep the legacy IP fallback.
func wsDeriveUploadOwner(w http.ResponseWriter, r *http.Request, h *Hub, ip string) (owner string, authenticated bool, ok bool) {
	if h.dashToken == "" {
		// No-token mode: every connection is authenticated; uploadOwner derives
		// from nz_anon. Only honour an inbound cookie matching mintAnonCookie's
		// wire shape (32 lowercase hex): a co-NAT attacker can set nz_anon to any
		// value, so a malformed one falls through to a fresh server-minted label
		// and the owner is always rooted in server-generated bytes (#485).
		if cookie, err := r.Cookie(anonCookieName); err == nil && isValidAnonCookieValue(cookie.Value) {
			// Sliding renewal on the handshake too: the upgrade is often the FIRST
			// request of a returning tab; HandleUpgrade forwards Set-Cookie into
			// the 101.
			renewAnonCookie(w, r, h.auth, cookie.Value)
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
		// AuthHandlers not wired (test harness only): legacy IP fallback. In
		// trusted-proxy mode an XFF-less request yields ip=="" and reserveOwnerSlot
		// treats "" as the cap-exempt bucket, so substitute a per-request random
		// owner so each such connection is subject to the normal cap (#2343).
		if ip == "" {
			var b [16]byte
			if _, err := rand.Read(b[:]); err != nil {
				// Entropy failed: refuse rather than fall back to the shared "" bucket.
				slog.Warn("ws upgrade: rand.Read failed deriving anon owner; refusing upgrade", "err", err)
				w.Header().Set("Retry-After", "30")
				http.Error(w, "could not derive upload owner; please retry", http.StatusServiceUnavailable)
				return "", false, false
			}
			return hex.EncodeToString(b[:]), true, true
		}
		return ip, true, true
	}
	// Token mode: only the auth-cookie path is examined here. Bearer-token WS
	// clients authenticate via the inner handleAuth message; until then the
	// client is unauthenticated with empty uploadOwner and never reaches
	// uploadStore.
	if cookie, err := r.Cookie(auth.AuthCookieName); err == nil {
		// h.cookieMAC is a getter so RotateCookieGen invalidations reach the next
		// upgrade (#1398). Local var so the compare and empty-guard see one value.
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
		// Advisory RetryAfter matches the HTTP /api/auth/login 429 branch so WS
		// and HTTP lockouts surface identical countdowns.
		c.SendJSON(wsproto.NewAuthFail(wsproto.AuthFail{

			Error:      "too many attempts",
			RetryAfter: wsAuthRetryAfterSeconds,
		}))
		// Rate-limited auth_fail counts toward the blended auth_fail metric plus
		// the dedicated rate-limited split so operators can tell a looping client
		// from a credential spray pacing under the limiter.
		serverMetrics.WSAuthFail()
		serverMetrics.WSAuthFailRateLimited()
		return
	}
	// Short-circuit when the connection is already authenticated via cookie —
	// do not touch msg.Token or run the ConstantTimeCompare so the
	// cookie-authed and token-authed paths are cleanly separated.
	if c.authenticated.Load() {
		c.SendRaw([]byte(wsproto.RawAuthOK))
		return
	}
	// Pre-hash both sides to normalize length — subtle.ConstantTimeCompare
	// returns 0 immediately on length mismatch, leaking the token length via
	// latency. Mirrors the HTTP Bearer path in dashboard_auth.go.
	tokenOK := false
	if h.dashToken != "" {
		got := sha256.Sum256([]byte(msg.Token))
		// h.dashTokenHash is precomputed at Hub construction.
		tokenOK = subtle.ConstantTimeCompare(got[:], h.dashTokenHash[:]) == 1
	}
	if h.dashToken == "" || tokenOK {
		// Distinguish "no token configured" from "valid token" in logs.
		if h.dashToken == "" {
			slog.Debug("ws auth: no-token mode, authenticating unconditionally")
		}
		// Derive uploadOwner from the token so WS token-auth enforces the same
		// per-owner upload quota as HTTP Bearer (uploadOwner "" matches every ""
		// owner in the store). Re-key the per-owner conn slot BEFORE flipping
		// authenticated so reserve/release stay paired on the SAME owner; a
		// reserve failure refuses the auth rather than admit an unaccounted slot (#1775).
		oldOwner := c.uploadOwnerKey()
		if oldOwner == "" && msg.Token != "" {
			// 128-bit owner key, parity with HTTP (dashboard_send.go ownerKeyFromCookie).
			sum := sha256.Sum256([]byte(msg.Token))
			newOwner := hex.EncodeToString(sum[:16])
			// rekeyOwnerSlot swaps release(old) → reserve(new) → setUploadOwner(new)
			// atomically under connCountByOwnerMu — the lock unregister holds while
			// reading the owner key — so teardown cannot release the wrong owner (#1808).
			if !h.rekeyOwnerSlot(c, oldOwner, newOwner) {
				// newOwner is at the per-owner ceiling; rekeyOwnerSlot left the
				// slot on oldOwner. Refuse: owner stays oldOwner, closing the
				// conn unwinds the pumps which unregister against oldOwner.
				c.SendRaw([]byte(wsproto.RawAuthFailInvalid))
				serverMetrics.WSAuthFail()
				if c.conn != nil {
					_ = c.conn.Close()
				}
				return
			}
		}
		c.authenticated.Store(true)
		// Mirror the auth flip into h.authClients for broadcastToAuthenticated
		// (#1409). The Store above must precede the mirror write so a concurrent
		// broadcast that sees the mirror entry also sees authenticated==true.
		h.markAuthenticated(c)
		c.SendRaw([]byte(wsproto.RawAuthOK))
	} else {
		c.SendRaw([]byte(wsproto.RawAuthFailInvalid))
		// The dedicated invalid-token split distinguishes credential spray from
		// throttling storms.
		serverMetrics.WSAuthFail()
		serverMetrics.WSAuthFailInvalidToken()
	}
}
