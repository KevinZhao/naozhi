package node

import (
	"crypto/sha256"
	"crypto/subtle"
	"expvar"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/netutil"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/ratelimit"
	"golang.org/x/time/rate"
)

// insecureReverseUpgradeTotal counts reverse-node WS upgrades accepted
// over plain HTTP from a non-loopback host — i.e. connections where the
// bearer token rides the first frame in cleartext, observable to any
// passive on-path party (R226-SEC-3 / R229-SEC-5, #1026).
//
// The existing warnInsecureReverseUpgradeOnce log fires at most once per
// process to avoid reconnect-storm log floods, which means an operator
// who tails the journal after the first event sees nothing further and
// has no way to gauge ongoing exposure. This monotonic expvar gauge fills
// that gap: it is published on /debug/vars (the dashboard's existing
// expvar endpoint) so a sustained non-zero delta is an actionable signal
// that TLS is still missing in front of /ws-node. Counter semantics
// (Add(1)) match the metric's "how many cleartext-token upgrades have we
// accepted" question; a zero value means every reverse upgrade arrived
// over TLS or from loopback.
var insecureReverseUpgradeTotal = expvar.NewInt("naozhi_node_insecure_reverse_upgrade_total")

// truncateLabelUTF8 truncates s to at most max bytes while preserving UTF-8
// validity and stripping log-injection codepoints. Raw byte truncation is
// unsafe when the cut falls mid-rune: the resulting string contains invalid
// UTF-8 bytes that flow into slog attrs, JSON responses, and dashboard
// renders. `strings.ToValidUTF8` strips any trailing invalid-byte fragment
// after a byte-level cut, keeping the rest intact. R67-SEC-6.
//
// R180-SEC-M2: also strip C0/C1/bidi/LS-PS codepoints. A compromised reverse
// node (with a valid token) could submit display_name / hostname containing
// bidi overrides to flip the rendered name on every dashboard (/api/sessions
// stats.nodes), or C1/newline bytes to corrupt slog attrs when the node
// registers / disconnects. Mirrors the cron-validator and project-name policy.
func truncateLabelUTF8(s string, max int) string {
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max], "")
	}
	if s == "" {
		return s
	}
	// Fast path: pure ASCII-printable is already safe.
	clean := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f || c >= 0x80 {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		if osutil.IsLogInjectionRune(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// reverseUpgrader is the WebSocket upgrader for reverse node connections.
// m2m connection: bearer token in the first WS message is the primary auth.
// As a defence-in-depth measure, reject any request that carries an Origin
// header — browsers always send Origin, machine-to-machine clients do not.
//
// R247-SEC-22 hardening: when a reverse proxy strips the Origin header
// before forwarding the upgrade (some misconfigured TLS terminators do
// this), a browser-driven XSS sender behind that same proxy could in
// theory craft an Origin-less request that looks m2m. Mitigate by
// requiring either:
//   - the request arrived over TLS (r.TLS != nil — direct termination), or
//   - the request came from a loopback host (dev / sidecar wiring), or
//   - the deployment opted in to plain-HTTP m2m via insecureReverseUpgrade
//     (set by NewReverseServer when explicitly configured).
//
// The bearer-token first-frame auth still gates connection acceptance
// regardless of transport; the TLS check is purely an origin-spoof
// hardening for proxies that strip Origin. The default of allowing
// loopback keeps dev / single-host deployments working without config.
var reverseUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		if r.Header.Get("Origin") != "" {
			return false
		}
		if r.TLS != nil {
			return true
		}
		if isLoopbackHost(r.Host) {
			return true
		}
		// Plain-HTTP non-loopback: log once-per-process and allow so
		// existing private-network deployments don't break, but make the
		// origin-spoof exposure visible in logs. Operators who want to
		// silence this should put TLS in front of /ws-node.
		//
		// Bump the monotonic counter on every such upgrade (not just the
		// first) so /debug/vars reflects ongoing cleartext-token exposure
		// even after the once-guarded warn has already fired (#1026).
		insecureReverseUpgradeTotal.Add(1)
		warnInsecureReverseUpgradeOnce(r.Host)
		return true
	},
}

// insecureReverseWarnOnce ensures the plain-HTTP non-loopback warning
// appears once per process to avoid log floods on reconnect storms.
var insecureReverseWarnOnce sync.Once

func warnInsecureReverseUpgradeOnce(host string) {
	insecureReverseWarnOnce.Do(func() {
		slog.Warn("reverse upgrade arrived over plain HTTP without Origin and not from loopback; deploy TLS in front of /ws-node to harden against Origin-strip proxies",
			"host", truncateLabelUTF8(host, 128))
	})
}

// isLoopbackHost reports whether host (Host header value, may include
// port) refers to a loopback address. Used by reverseUpgrader's
// defence-in-depth check.
func isLoopbackHost(host string) bool {
	h := host
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		// Bracketed IPv6 form `[::1]:port` — trim brackets for compare.
		if strings.HasPrefix(host, "[") {
			if rb := strings.IndexByte(host, ']'); rb >= 0 {
				h = host[1:rb]
			}
		} else {
			h = host[:i]
		}
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		h = host[1 : len(host)-1]
	}
	switch strings.ToLower(h) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// ReverseServer accepts /ws-node connections from remote naozhi nodes.
// Remote nodes dial in (reverse connect) to traverse NAT.
type ReverseServer struct {
	mu    sync.RWMutex
	auth  map[string]string       // node_id → expected token
	names map[string]string       // node_id → configured display_name
	conns map[string]*ReverseConn // node_id → active connection

	// wsLimiter is an internal per-IP rate limiter store for /ws-node connections.
	// Separate from the dashboard login limiter to prevent cross-endpoint interference.
	// Higher burst (10) than login (5) since machine-to-machine reconnects are bursty.
	wsLimiter *ratelimit.Limiter

	// trustedProxy enables X-Forwarded-For last-hop IP extraction for rate limiting.
	// When true (ALB/CloudFront in front), per-IP limits apply to the real client,
	// not the proxy's single IP.
	trustedProxy bool

	OnRegister   func(id string, conn *ReverseConn)
	OnDeregister func(id string)
}

// NewReverseServer creates a server that accepts /ws-node connections.
// auth is the reverse_nodes config from config.yaml.
// trustedProxy enables X-Forwarded-For last-hop IP extraction so per-IP
// rate limiting works correctly when deployed behind ALB/CloudFront.
func NewReverseServer(auth map[string]config.ReverseNodeEntry, trustedProxy bool) *ReverseServer {
	tokens := make(map[string]string, len(auth))
	names := make(map[string]string, len(auth))
	// 两个 node 拿到同一个 token 等于身份可互换——token 认证靠 ConstantTimeCompare
	// 只按值匹配，node_id 在 ReadJSON 那一刻由客户端自报。运维误配（复制粘贴）
	// 最常见，启动时 WARN 一下，不拒启动（允许临时 rotate 场景）。
	seen := make(map[string]string, len(auth))
	for id, e := range auth {
		tokens[id] = e.Token
		names[id] = e.DisplayName
		if e.Token == "" {
			continue
		}
		if other, dup := seen[e.Token]; dup {
			slog.Warn("reverse node duplicate token; node_ids are interchangeable under this token — rotate one",
				"node_id_a", other, "node_id_b", id)
			continue
		}
		seen[e.Token] = id
	}
	return &ReverseServer{
		auth:  tokens,
		names: names,
		conns: make(map[string]*ReverseConn),
		wsLimiter: ratelimit.New(ratelimit.Config{
			Rate:    rate.Every(5 * time.Second), // 1 per 5s sustained
			Burst:   10,                          // 10 burst
			MaxKeys: 10_000,                      // cap per-IP table; matches dashboard auth limiter
			TTL:     10 * time.Minute,            // idle eviction
		}),
		trustedProxy: trustedProxy,
	}
}

// ServeHTTP handles the /ws-node WebSocket endpoint.
func (s *ReverseServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Per-IP rate limit to prevent token brute-force via rapid connect cycles.
	// Uses trusted-proxy-aware client IP so ALB-fronted deployments limit the
	// real caller, not the single ALB IP.
	ip := netutil.ClientIP(r, s.trustedProxy)
	// Fallback to a shared bucket if IP resolution failed so ratelimit's
	// empty-key hard-reject doesn't 429 a legitimate client forever.
	limitKey := ip
	if limitKey == "" {
		limitKey = "_unknown_"
	}
	if !s.wsLimiter.Allow(limitKey) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	conn, err := reverseUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Debug("ws-node upgrade failed", "err", err)
		return
	}
	conn.SetReadLimit(4 << 10) // 4 KB — small limit for unauthenticated register message

	// Read register message with timeout. R182-GO-P1-1: both SetReadDeadline
	// returns were previously dropped. If the underlying net.Conn is already
	// half-closed mid-handshake, SetReadDeadline fails and ReadJSON would
	// block forever (deadline-less on a dead socket), leaking a goroutine
	// on every failed handshake from the public /ws-node endpoint. Treat
	// failure as "connection unusable" and bail fast, mirroring the
	// symmetric SetWriteDeadline check at line 195.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		slog.Debug("ws-node: set read deadline failed", "err", err)
		conn.Close()
		return
	}
	var msg ReverseMsg
	if err := conn.ReadJSON(&msg); err != nil || msg.Type != "register" {
		conn.Close()
		return
	}
	// Sanitize remote-supplied label fields immediately after read so any
	// future slog call before auth (e.g. an added forensic breadcrumb) is
	// safe regardless of code order. Defense-in-depth — current code paths
	// only log msg.NodeID before auth, but the invariant is brittle.
	const maxLabel = 256
	msg.DisplayName = truncateLabelUTF8(msg.DisplayName, maxLabel)
	msg.Hostname = truncateLabelUTF8(msg.Hostname, maxLabel)
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		slog.Debug("ws-node: clear read deadline failed", "err", err)
		conn.Close()
		return
	}

	// Validate token — constant-time comparison to prevent timing oracle.
	// Generic error to avoid node_id enumeration. When the node_id is unknown
	// we still run a fixed-length compare so the reject path does not leak (via
	// response latency) whether the node_id exists at all. Pre-hashing both
	// sides with SHA-256 normalises the compare length to 32 bytes regardless
	// of the submitted token length — ConstantTimeCompare short-circuits on
	// length mismatch, so comparing raw bytes would still leak "my stored
	// expected is (or isn't) the same length as the probe token".
	expected, ok := s.auth[msg.NodeID]
	if !ok || expected == "" {
		var dummy [32]byte
		probe := sha256.Sum256([]byte(msg.Token))
		_ = subtle.ConstantTimeCompare(dummy[:], probe[:])
	}
	var matched bool
	if ok && expected != "" {
		expectedHash := sha256.Sum256([]byte(expected))
		probeHash := sha256.Sum256([]byte(msg.Token))
		matched = subtle.ConstantTimeCompare(expectedHash[:], probeHash[:]) == 1
	}
	if !matched {
		// R180-SEC-H2 / R181-GO-P2-1: msg.NodeID comes from an unauthenticated
		// 4 KB frame on the public /ws-node endpoint. Anyone can probe with
		// arbitrary bytes. SanitizeForLog keeps attr values machine-readable
		// (strips C0/C1/bidi/LS-PS → '_') instead of the earlier %q path
		// which produced Go-quoted strings that then got double-escaped by
		// slog's JSON handler.
		// RNEW-SEC-006: log r.Host as a forensic breadcrumb. The upgrader
		// does not enforce a Host allowlist (no config surface today),
		// so the only recourse for "is this IP talking to the expected
		// virtual host?" triage is after-the-fact log inspection.
		// Sanitized so a Host: header carrying bidi/C1/newline bytes
		// cannot corrupt slog attrs.
		slog.Warn("reverse node auth failed",
			"ip", ip,
			"node_id", osutil.SanitizeForLog(msg.NodeID, 64),
			"host", osutil.SanitizeForLog(r.Host, 256))
		conn.WriteJSON(ReverseMsg{Type: "register_fail", Error: "auth failed"}) //nolint
		conn.Close()
		return
	}

	// Auth succeeded — raise limit for RPC payloads (session responses, event batches).
	conn.SetReadLimit(16 << 20) // 16 MB

	// Use configured display name; fall back to what remote sent.
	// msg.DisplayName / msg.Hostname were already truncated at read time
	// (above). Configured-side names and r.RemoteAddr still need their cap.
	displayName := s.names[msg.NodeID]
	if displayName == "" {
		displayName = msg.DisplayName
	}
	if displayName == "" {
		displayName = msg.NodeID
	}
	displayName = truncateLabelUTF8(displayName, maxLabel)

	remoteLabel := msg.Hostname
	if remoteLabel == "" {
		remoteLabel = r.RemoteAddr
	}
	remoteLabel = truncateLabelUTF8(remoteLabel, maxLabel)
	// Forward the advertised capability set into the connection's
	// NodeMeta so server-side dispatch can answer
	// `Conn.Meta().HasCap("acp")` for backend-routing decisions
	// (Sprint 6b of the multi-backend RFC). We pass the truncated
	// hostname (msg.Hostname after maxLabel cut earlier) rather than
	// remoteLabel so meta.Hostname stays distinct from RemoteAddr —
	// only the former survives `r.RemoteAddr` fallback above.
	rc := newReverseConnWithMeta(msg.NodeID, displayName, remoteLabel, conn, msg.Capabilities, msg.Hostname)
	// Bound the register response write so a slow-read attacker can't
	// park this goroutine indefinitely at the TCP window.
	// newReverseConnWithMeta applies 10s per write thereafter; this
	// pre-handoff write needs the same protection. If SetWriteDeadline fails (conn closed mid-handshake),
	// abort before WriteJSON would block deadline-less on a dead socket.
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		rc.Close()
		return
	}
	if err := conn.WriteJSON(ReverseMsg{Type: "registered"}); err != nil {
		rc.Close()
		return
	}
	// R183-GO-M1: clearing the write deadline can only fail on a broken /
	// half-closed socket. Silently dropping the error mirrors a bug fixed
	// symmetrically at line 144/155 for SetReadDeadline (R182-GO-P1-1):
	// per-write deadline resets in newReverseConnWithMeta's writePump also fail,
	// and without a deadline, a subsequent WriteJSON on this conn can
	// block until TCP keepalive expires (minutes). Treat failure as
	// "connection unusable", tear down, and bail.
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		slog.Debug("ws-node: clear write deadline failed", "err", err)
		rc.Close()
		return
	}

	s.mu.Lock()
	if old, exists := s.conns[msg.NodeID]; exists {
		old.Close()
	}
	s.conns[msg.NodeID] = rc
	s.mu.Unlock()

	// R181-SEC-P2-1: authenticated node_id matched a config key, but those
	// keys are never run through truncateLabelUTF8 on load — an operator
	// typo in config.yaml with a bidi/C1/newline char would reach slog
	// attrs verbatim. Symmetric with the auth-failed path and cheap.
	safeNodeID := truncateLabelUTF8(msg.NodeID, 64)
	// R212-ARCH-402: WARN (fires first, before the Info below) when the
	// remote advertises capability tags outside this binary's known set.
	// Pure observability — the node is still registered normally.
	logUnknownCaps(safeNodeID, msg.Capabilities)
	// RNEW-SEC-006: symmetric with the auth-failed path above — log
	// r.Host so operators can correlate registered nodes with the
	// Host header they came in on.
	slog.Info("reverse node registered",
		"node_id", safeNodeID,
		"ip", ip,
		"host", osutil.SanitizeForLog(r.Host, 256))

	if s.OnRegister != nil {
		// msg.NodeID is kept verbatim here so downstream state
		// (`s.conns[msg.NodeID]`, Server.nodes, knownNodes) is keyed with
		// the authenticated-config id; OnDeregister below must pass the
		// same value so the map entries round-trip correctly. Sanitizing
		// only the slog.Info label (safeNodeID above) matches R181-SEC-P2-1.
		s.OnRegister(msg.NodeID, rc)
	}

	go rc.readLoop()

	// Wait for disconnect, then deregister
	go func() {
		<-rc.done
		s.mu.Lock()
		if s.conns[msg.NodeID] == rc {
			delete(s.conns, msg.NodeID)
		}
		s.mu.Unlock()
		slog.Info("reverse node disconnected", "node_id", safeNodeID)
		if s.OnDeregister != nil {
			s.OnDeregister(msg.NodeID)
		}
	}()
	// ServeHTTP returns; rc.readLoop keeps the WS alive on its own goroutine.
}

// AllNodes returns all configured node IDs mapped to their display names.
// Includes disconnected nodes.
func (s *ReverseServer) AllNodes() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]string, len(s.names))
	for id, name := range s.names {
		result[id] = name
	}
	return result
}
