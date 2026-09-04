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
	"github.com/naozhi/naozhi/internal/envpolicy"
	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/netutil"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/ratelimit"
	"golang.org/x/time/rate"
)

// insecureReverseUpgradeTotal counts reverse-node upgrades accepted over plain
// HTTP from a non-loopback host (bearer token in cleartext on the first
// frame). The once-per-process warn cannot show ongoing exposure; this
// /debug/vars counter can (#1026).
var insecureReverseUpgradeTotal = expvar.NewInt("naozhi_node_insecure_reverse_upgrade_total")

// truncateLabelUTF8 truncates s to at most max bytes without producing invalid
// UTF-8 and strips C0/C1/bidi/LS-PS codepoints: a compromised reverse node
// could otherwise flip rendered names on every dashboard or corrupt slog attrs.
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

// reverseUpgrader upgrades reverse-node connections. The first-frame bearer
// token is the primary auth; as defence in depth any Origin header is rejected
// (browsers always send one, m2m clients never do), and a plain-HTTP upgrade
// is accepted only from loopback or a private-LAN Host — a proxy that strips
// Origin could otherwise let a browser-driven sender look m2m.
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
		// Plain-HTTP, non-loopback: count every occurrence, not just the first.
		insecureReverseUpgradeTotal.Add(1)
		// Private LAN: cleartext token is exposed only to that segment (the
		// documented no-TLS sidecar topology) — allow with a one-time warn.
		// Public/routable Host: the token would cross the open internet, so
		// HARD-REJECT (403 before any frame is read) (#1824).
		if isPrivateHost(r.Host) {
			warnInsecureReversePrivateOnce(r.Host)
			return true
		}
		warnInsecureReverseUpgradeOnce(r.Host)
		return false
	},
}

// insecureReverseWarnOnce bounds the public-host warning to once per process
// (reconnect storms).
var insecureReverseWarnOnce sync.Once

func warnInsecureReverseUpgradeOnce(host string) {
	insecureReverseWarnOnce.Do(func() {
		slog.Warn("reverse upgrade over plain HTTP from a public/routable host REJECTED; the bearer token would ride the first frame in cleartext over the open network — deploy TLS in front of /ws-node (loopback/private-LAN topologies are unaffected)",
			"host", truncateLabelUTF8(host, 128))
	})
}

// insecureReversePrivateWarnOnce is separate so each host class gets its own
// once-per-process line instead of one masking the other.
var insecureReversePrivateWarnOnce sync.Once

func warnInsecureReversePrivateOnce(host string) {
	insecureReversePrivateWarnOnce.Do(func() {
		slog.Warn("reverse upgrade arrived over plain HTTP without Origin from a private LAN address; the bearer token is sent in cleartext and readable by passive listeners on the same segment — put TLS in front of /ws-node if the segment is not fully trusted",
			"host", truncateLabelUTF8(host, 128))
	})
}

// isPrivateHost reports whether a Host header value is an RFC1918 / ULA /
// link-local UNICAST literal; non-IP hostnames are non-private. Ranges come
// from envpolicy.ClassifyHost (#2300), but the POLICY is deliberately narrower
// than the outbound SSRF guards: here "private" means "accept plaintext with a
// warning", so classifying more ranges as private would LOOSEN the gate.
func isPrivateHost(host string) bool {
	k, ok := envpolicy.ClassifyHost(stripHostPort(host))
	if !ok {
		return false
	}
	return k.Any(envpolicy.IPPrivate | envpolicy.IPLinkLocalUnicast)
}

// stripHostPort reduces a Host header to its bare host: "[::1]:8080" → "::1",
// "10.0.0.1:80" → "10.0.0.1"; a bare unbracketed IPv6 literal is left intact (#2339).
func stripHostPort(host string) string {
	h := host
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		if strings.HasPrefix(host, "[") {
			if rb := strings.IndexByte(host, ']'); rb >= 0 {
				h = host[1:rb]
			}
		} else if strings.IndexByte(host, ':') == i {
			// Exactly one colon → host:port (hostname or IPv4).
			h = host[:i]
		}
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		h = host[1 : len(host)-1]
	}
	return h
}

// isLoopbackHost reports whether a Host header value (may include port) is a
// loopback address — the whole 127.0.0.0/8 range and ::1, not just the literals.
func isLoopbackHost(host string) bool {
	h := stripHostPort(host)
	if strings.ToLower(h) == "localhost" {
		return true
	}
	k, ok := envpolicy.ClassifyHost(h)
	return ok && k.Has(envpolicy.IPLoopback)
}

// ReverseServer accepts /ws-node connections from remote naozhi nodes.
// Remote nodes dial in (reverse connect) to traverse NAT.
type ReverseServer struct {
	mu    sync.RWMutex
	names map[string]string       // node_id → configured display_name
	conns map[string]*ReverseConn // node_id → active connection

	// authHash holds sha256(expected token) per node_id, precomputed so the
	// auth path hashes only the inbound probe. Empty-token entries are omitted.
	authHash map[string][32]byte

	// wsLimiter is a per-IP limiter for /ws-node, separate from the dashboard
	// login limiter; higher burst because m2m reconnects are bursty.
	wsLimiter *ratelimit.Limiter

	// trustedProxy enables X-Forwarded-For last-hop IP extraction so per-IP
	// limits apply to the real client behind ALB/CloudFront.
	trustedProxy bool

	OnRegister   func(id string, conn *ReverseConn)
	OnDeregister func(id string)

	// testHookBeforeAck runs after rc is installed into s.conns and before the
	// "registered" ack. Tests only: widens the insert→ack window (#2458).
	testHookBeforeAck func(rc *ReverseConn)
}

// ReverseNodeAuth is the zero-dependency auth entry for one allowed
// reverse-connecting node; the cmd boundary translates config.ReverseNodeEntry
// into it so internal/node does not import internal/config (#1411).
type ReverseNodeAuth struct {
	Token       string
	DisplayName string
}

// NewReverseServer creates a server that accepts /ws-node connections; auth
// maps node_id → ReverseNodeAuth, trustedProxy enables X-Forwarded-For.
func NewReverseServer(auth map[string]ReverseNodeAuth, trustedProxy bool) *ReverseServer {
	names := make(map[string]string, len(auth))
	hashes := make(map[string][32]byte, len(auth))
	// 两个 node 共用一个 token 等于身份可互换（node_id 由客户端自报）；启动时 WARN，
	// 不拒启动（允许临时 rotate）。
	seen := make(map[string]string, len(auth))
	for id, e := range auth {
		names[id] = e.DisplayName
		if e.Token == "" {
			continue
		}
		hashes[id] = sha256.Sum256([]byte(e.Token))
		if other, dup := seen[e.Token]; dup {
			slog.Warn("reverse node duplicate token; node_ids are interchangeable under this token — rotate one",
				"node_id_a", other, "node_id_b", id)
			continue
		}
		seen[e.Token] = id
	}
	return &ReverseServer{
		names:    names,
		authHash: hashes,
		conns:    make(map[string]*ReverseConn),
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
	// Per-IP rate limit against token brute-force via rapid connect cycles.
	ip := netutil.ClientIP(r, s.trustedProxy)
	// Shared bucket when IP resolution failed, so the limiter's empty-key
	// hard-reject does not 429 a legitimate client forever.
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

	// A failed SetReadDeadline means a half-closed socket; a deadline-less
	// ReadJSON would leak a goroutine per failed handshake on this public endpoint.
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
	// Sanitize remote labels immediately so any pre-auth slog call is safe
	// regardless of code order.
	const maxLabel = 256
	msg.DisplayName = truncateLabelUTF8(msg.DisplayName, maxLabel)
	msg.Hostname = truncateLabelUTF8(msg.Hostname, maxLabel)
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		slog.Debug("ws-node: clear read deadline failed", "err", err)
		conn.Close()
		return
	}

	// Constant-time token check with a generic error so neither timing nor the
	// message reveals whether node_id exists. Both sides are SHA-256 hashed so
	// the compare length is fixed (ConstantTimeCompare short-circuits on length
	// mismatch); an unknown node_id still runs a dummy compare.
	expectedHash, ok := s.authHash[msg.NodeID]
	probeHash := sha256.Sum256([]byte(msg.Token))
	if !ok {
		var dummy [32]byte
		_ = subtle.ConstantTimeCompare(dummy[:], probeHash[:])
	}
	matched := ok && subtle.ConstantTimeCompare(expectedHash[:], probeHash[:]) == 1
	if !matched {
		// node_id and Host come from an unauthenticated frame on a public
		// endpoint; sanitize. r.Host is the only forensic breadcrumb for
		// "which virtual host was this IP probing" (no Host allowlist exists).
		slog.Warn("reverse node auth failed",
			"ip", ip,
			"node_id", osutil.SanitizeForLog(msg.NodeID, 64),
			"host", osutil.SanitizeForLog(r.Host, 256))
		conn.WriteJSON(ReverseMsg{Type: "register_fail", Error: "auth failed"}) //nolint
		conn.Close()
		return
	}

	// Authenticated: one RPC frame carries at most one stream-json line (#2084).
	conn.SetReadLimit(limits.MaxStreamJSONLine)

	// Configured display name wins; configured-side names and r.RemoteAddr
	// still need the cap.
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
	// msg.Hostname (not remoteLabel) keeps meta.Hostname distinct from RemoteAddr.
	rc := newReverseConnWithMeta(msg.NodeID, displayName, remoteLabel, conn, msg.Capabilities, msg.Hostname)
	// Install into s.conns BEFORE the "registered" ack: the client may redial
	// the instant it sees the ack, and ack-first ordering let a fast reconnect
	// be overwritten by the stale handler ("online but invisible", #2458).
	// Invariant: s.conns[id] is the conn that most recently reached this point;
	// it closes the previous owner, whose deregister/rollback paths see
	// owns == false and touch nothing. Lock order: s.mu → ReverseConn.closeMu;
	// no callback runs under s.mu.
	s.mu.Lock()
	old, displaced := s.conns[msg.NodeID]
	if displaced {
		old.Close()
	}
	s.conns[msg.NodeID] = rc
	s.mu.Unlock()

	// abortAck rolls the insert back (identity-checked: a newer conn may own
	// the id). If we displaced an old conn, its own deregister was suppressed
	// by that check, so emit OnDeregister on its behalf.
	abortAck := func() {
		s.mu.Lock()
		owns := s.conns[msg.NodeID] == rc
		if owns {
			delete(s.conns, msg.NodeID)
		}
		s.mu.Unlock()
		rc.Close()
		if owns && displaced && s.OnDeregister != nil {
			s.OnDeregister(msg.NodeID)
		}
	}

	if s.testHookBeforeAck != nil {
		s.testHookBeforeAck(rc)
	}

	// Bounded so a slow-read attacker cannot park this goroutine at the TCP
	// window; a failed SetWriteDeadline means the conn is already dead.
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		abortAck()
		return
	}
	if err := conn.WriteJSON(ReverseMsg{Type: "registered"}); err != nil {
		abortAck()
		return
	}
	// Clearing the deadline only fails on a broken socket, and a later
	// WriteJSON on it could block until TCP keepalive expires.
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		slog.Debug("ws-node: clear write deadline failed", "err", err)
		abortAck()
		return
	}

	// Config keys are never sanitized on load; an operator typo with bidi/C1
	// bytes must not reach slog verbatim.
	safeNodeID := truncateLabelUTF8(msg.NodeID, 64)
	// Observability only — the node is still registered normally.
	logUnknownCaps(safeNodeID, msg.Capabilities)

	// A newer conn that displaced us already owns OnRegister and closed rc.
	s.mu.RLock()
	stillOwns := s.conns[msg.NodeID] == rc
	s.mu.RUnlock()
	if !stillOwns {
		slog.Debug("reverse node superseded before register", "node_id", safeNodeID, "ip", ip)
	} else {
		// r.Host correlates registered nodes with the Host header they used.
		slog.Info("reverse node registered",
			"node_id", safeNodeID,
			"ip", ip,
			"host", osutil.SanitizeForLog(r.Host, 256))
	}
	if stillOwns && s.OnRegister != nil {
		// Verbatim msg.NodeID: downstream maps are keyed by the config id and
		// OnDeregister must pass the same value.
		s.OnRegister(msg.NodeID, rc)
	}

	go rc.readLoop()

	go func() {
		<-rc.done
		s.mu.Lock()
		// Only the current owner may tear down shared state: a stale
		// deregister of a replaced conn would wipe the freshly reconnected
		// live node from Server.nodes until the next full reconnect.
		owns := s.conns[msg.NodeID] == rc
		if owns {
			delete(s.conns, msg.NodeID)
		}
		s.mu.Unlock()
		slog.Info("reverse node disconnected", "node_id", safeNodeID)
		if owns && s.OnDeregister != nil {
			s.OnDeregister(msg.NodeID)
		}
	}()
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
