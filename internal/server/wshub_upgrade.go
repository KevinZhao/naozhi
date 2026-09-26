package server

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/wsproto"
)

// wsAuthRetryAfterSeconds is the advisory "try again in N seconds" value in
// the WS auth_fail rate-limit reply. It mirrors the HTTP /api/auth/login
// Retry-After (60s) so the front-end shares one countdown helper; the limiter
// refills a token every 12s (burst=5), so 60s avoids a back-to-back 429 loop.
const wsAuthRetryAfterSeconds = 60

func (h *Hub) HandleUpgrade(w http.ResponseWriter, r *http.Request) {
	// The handshake has its own per-IP bucket, so tab-reload / mobile-wake
	// bursts do not spend the auth-attempt budget handleAuth draws from.
	if !h.admit.allowUpgrade(r) {
		errRespRetry(w, http.StatusTooManyRequests, "rate_limited", "too many requests", wsAuthRetryAfterSeconds)
		return
	}
	if !h.admit.reserveConn() {
		http.Error(w, "too many WebSocket connections", http.StatusServiceUnavailable)
		return
	}
	// Release the reserved slot on any pre-register failure path.
	slotReleased := false
	defer func() {
		if !slotReleased {
			h.admit.releaseConn(1)
		}
	}()

	// Derive uploadOwner BEFORE upgrader.Upgrade: Set-Cookie cannot be added
	// once the response is hijacked into a 101. Mint failure refuses the
	// upgrade (503) so co-NAT clients never share an IP-derived owner (#1326).
	ip := h.admit.clientIP(r)
	uploadOwnerKey, preAuthenticated, ok := h.admit.deriveUploadOwner(w, r, ip)
	if !ok {
		return
	}

	// gorilla's Upgrade writes the 101 itself; headers set on w (mintAnonCookie /
	// renewAnonCookie) are DROPPED unless forwarded via responseHeader.
	var respHdr http.Header
	if sc := w.Header().Values("Set-Cookie"); len(sc) > 0 {
		respHdr = http.Header{"Set-Cookie": sc}
	}
	conn, err := h.admit.upgrade(w, r, respHdr)
	if err != nil {
		// Origin + remote IP let operators diagnose CheckOrigin rejections.
		slog.Debug("ws upgrade failed",
			"err", err,
			"remote", ip,
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
	if !h.admit.reserveOwner(c.uploadOwnerKey()) {
		// Close so the client sees a clean RST; no CloseFrame, to avoid allocating
		// the per-conn write buffer at an exhausted boundary.
		conn.Close()
		return
	}
	ownerSlotHeld = true
	defer func() {
		if ownerSlotHeld && !slotReleased {
			h.admit.releaseOwner(c.uploadOwnerKey())
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

func (h *Hub) handleAuth(c *wsClient, msg node.ClientMsg) {
	// Per-IP rate limit to prevent brute-force via rapid connect/auth/disconnect cycles.
	if !h.admit.allowAuthAttempt(c.remoteIP) {
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
	if !h.admit.tokenMode() || h.admit.tokenMatches(msg.Token) {
		// Distinguish "no token configured" from "valid token" in logs.
		if !h.admit.tokenMode() {
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
			// rekeyOwner swaps release(old) → reserve(new) → setUploadOwner(new)
			// under the lock unregister holds while reading the owner key, so
			// teardown cannot release the wrong owner.
			if !h.admit.rekeyOwner(c, oldOwner, newOwner) {
				// newOwner is at the per-owner ceiling; rekeyOwner left the
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
		// The Store above precedes this so a broadcast that finds c in the
		// authenticated set also sees authenticated==true.
		h.subs.markAuthenticated(c)
		c.SendRaw([]byte(wsproto.RawAuthOK))
	} else {
		c.SendRaw([]byte(wsproto.RawAuthFailInvalid))
		// The dedicated invalid-token split distinguishes credential spray from
		// throttling storms.
		serverMetrics.WSAuthFail()
		serverMetrics.WSAuthFailInvalidToken()
	}
}
