package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
)

// maxWSConns caps simultaneous WebSocket upgrades; the broadcast pool is
// sized from it.
const maxWSConns = 500

// maxConnsPerOwner is the per-uploadOwner sub-cap: room for one power user's
// tabs + integrations while a single stolen token cannot monopolise
// maxWSConns.
const maxConnsPerOwner = 20

// connAdmission decides who gets a WebSocket and how much each owner may use:
// the per-IP handshake and auth limiters, the credential checks, the global
// and per-owner connection caps, and the per-owner send budget. It owns its
// locks; the Hub reaches it only through these methods.
type connAdmission struct {
	// Fixed at construction.
	dashToken string
	// dashTokenHash is sha256(dashToken) for constant-time comparison;
	// rotating dashToken requires a restart.
	dashTokenHash [32]byte
	// cookieMAC is a getter, not a snapshot, so a cookie rotation reaches the
	// next upgrade without a Hub rebuild.
	cookieMAC func() string
	// auth, when set, mints the per-browser nz_anon cookie in no-token mode;
	// nil only in test harnesses, which fall back to an IP-derived owner.
	auth         *auth.Handlers
	trustedProxy bool // trust X-Forwarded-For for client IP extraction
	// upgradeLimiter gates the handshake, which fires legitimately on
	// tab-reload / mobile-wake; authLimiter gates the inner `auth` message
	// (a credential test). Separate buckets so reloads cannot lock out a
	// login. Either may be nil (no limit); both return true when allowed.
	upgradeLimiter func(ip string) bool
	authLimiter    func(ip string) bool
	upgrader       websocket.Upgrader

	// conns counts open connections for maxWSConns. Reserved with an Add
	// before the check so a concurrent burst cannot all see room and land
	// past the cap.
	conns atomic.Int64

	// owners counts connections per uploadOwner for maxConnsPerOwner; the
	// "" owner is exempt and never stored. Entries go at zero so the map is
	// bounded by active owners.
	ownersMu sync.Mutex
	owners   map[string]int

	// sendLimiters buckets the WS send budget by uploadOwner so N tabs
	// cannot multiply the per-connection burst N×.
	sendLimiters sync.Map // owner string -> *rate.Limiter
}

func newConnAdmission(opts HubOptions) *connAdmission {
	// Always a getter, so the upgrade path never calls a nil func.
	cookieMAC := opts.CookieMACFn
	if cookieMAC == nil {
		static := opts.CookieMAC
		cookieMAC = func() string { return static }
	}
	a := &connAdmission{
		dashToken:      opts.DashToken,
		cookieMAC:      cookieMAC,
		auth:           opts.Auth,
		trustedProxy:   opts.TrustedProxy,
		upgradeLimiter: opts.WSUpgradeLimiter,
		authLimiter:    opts.WSAuthLimiter,
		owners:         make(map[string]int),
	}
	if opts.DashToken != "" {
		a.dashTokenHash = sha256.Sum256([]byte(opts.DashToken))
	}
	a.upgrader = websocket.Upgrader{
		// Shared with the HTTP CSRF gate so both stay in lockstep (empty
		// Origin permitted, "null" rejected, X-Forwarded-Host under trustedProxy).
		CheckOrigin:     func(r *http.Request) bool { return auth.SameOriginOK(r, a.trustedProxy) },
		ReadBufferSize:  8192,
		WriteBufferSize: 8192,
	}
	return a
}

func (a *connAdmission) clientIP(r *http.Request) string {
	return clientIP(r, a.trustedProxy)
}

// allowUpgrade applies the per-IP handshake limit. In trusted-proxy mode an
// XFF-less request would collapse to the shared unknown-IP bucket and let one
// caller starve every other such caller, so it is refused outright, like the
// HTTP login. An empty IP is still checked: the limiter maps it to the shared
// bucket, and skipping it would let a malformed RemoteAddr bypass the limit.
func (a *connAdmission) allowUpgrade(r *http.Request) bool {
	if a.upgradeLimiter == nil {
		return true
	}
	if !requestHasResolvableClientIP(r, a.trustedProxy) {
		return false
	}
	return a.upgradeLimiter(a.clientIP(r))
}

// allowAuthAttempt applies the per-IP limit on the inner `auth` message.
func (a *connAdmission) allowAuthAttempt(ip string) bool {
	return a.authLimiter == nil || a.authLimiter(ip)
}

// tokenMode reports whether a dashboard token is configured; without one
// every connection is authenticated.
func (a *connAdmission) tokenMode() bool { return a.dashToken != "" }

// tokenMatches compares tok with the dashboard token in constant time. Both
// sides are hashed first: ConstantTimeCompare returns early on a length
// mismatch, which would leak the token length through latency.
func (a *connAdmission) tokenMatches(tok string) bool {
	if a.dashToken == "" {
		return false
	}
	got := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare(got[:], a.dashTokenHash[:]) == 1
}

// reserveConn takes a slot under maxWSConns; every success is paired with
// one releaseConn.
func (a *connAdmission) reserveConn() bool {
	if a.conns.Add(1) > maxWSConns {
		a.conns.Add(-1)
		return false
	}
	return true
}

func (a *connAdmission) releaseConn(n int) {
	a.conns.Add(int64(-n))
}

func (a *connAdmission) upgrade(w http.ResponseWriter, r *http.Request, respHdr http.Header) (*websocket.Conn, error) {
	return a.upgrader.Upgrade(w, r, respHdr)
}

// deriveUploadOwner runs the auth-cookie / nz_anon resolution BEFORE the
// upgrade, so any minted Set-Cookie rides the 101 response.
//
// It returns the uploadOwner key for the new client, whether the client
// starts authenticated (auth-cookie match in token mode, or no-token mode),
// and ok=false when the upgrade must be refused (the 503 has been written;
// callers return). In no-token mode without a valid nz_anon cookie it mints
// one and refuses if the entropy source fails, so co-NAT clients never share
// an IP-derived owner bucket.
func (a *connAdmission) deriveUploadOwner(w http.ResponseWriter, r *http.Request, ip string) (owner string, authenticated bool, ok bool) {
	if a.dashToken == "" {
		// No-token mode: every connection is authenticated; uploadOwner derives
		// from nz_anon. Only an inbound cookie matching mintAnonCookie's wire
		// shape (32 lowercase hex) is honoured: a co-NAT attacker can set
		// nz_anon to any value, so a malformed one falls through to a fresh
		// server-minted label and the owner is always rooted in server bytes.
		if cookie, err := r.Cookie(anonCookieName); err == nil && isValidAnonCookieValue(cookie.Value) {
			// Sliding renewal on the handshake too: the upgrade is often the
			// FIRST request of a returning tab.
			renewAnonCookie(w, r, a.auth, cookie.Value)
			return ownerKeyFromCookie(cookie.Value), true, true
		}
		if a.auth != nil {
			val, mintErr := mintAnonCookie(w, r, a.auth)
			if mintErr != nil {
				slog.Warn("ws upgrade: mintAnonCookie failed; refusing to fall back to IP-derived owner key",
					"err", mintErr, "remote", ip)
				w.Header().Set("Retry-After", "30")
				http.Error(w, "could not derive upload owner; please retry", http.StatusServiceUnavailable)
				return "", false, false
			}
			return ownerKeyFromCookie(val), true, true
		}
		// No auth handlers (test harness only): IP fallback. In trusted-proxy
		// mode an XFF-less request yields ip=="", which the owner cap treats as
		// exempt, so each such connection gets a random owner instead.
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
	// Token mode: only the auth cookie is examined here. Bearer-token clients
	// authenticate via the inner `auth` message; until then the client is
	// unauthenticated with an empty owner and never reaches uploadStore.
	if cookie, err := r.Cookie(auth.AuthCookieName); err == nil {
		// One read of the getter so the compare and the empty-guard agree.
		mac := a.cookieMAC()
		if mac != "" && subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(mac)) == 1 {
			// Same derivation as the HTTP uploadOwner, so files uploaded on one
			// transport can be claimed on the other.
			return ownerKeyFromCookie(cookie.Value), true, true
		}
	}
	return "", false, true
}

// reserveOwner takes a slot under maxConnsPerOwner, returning false at the
// cap. Every success is paired with a release at teardown. Owner "" always
// succeeds without being counted.
func (a *connAdmission) reserveOwner(owner string) bool {
	a.ownersMu.Lock()
	defer a.ownersMu.Unlock()
	return a.reserveOwnerLocked(owner)
}

func (a *connAdmission) reserveOwnerLocked(owner string) bool {
	if owner == "" {
		return true
	}
	if a.owners[owner] >= maxConnsPerOwner {
		return false
	}
	a.owners[owner]++
	return true
}

func (a *connAdmission) releaseOwner(owner string) {
	a.ownersMu.Lock()
	defer a.ownersMu.Unlock()
	a.releaseOwnerLocked(owner)
}

func (a *connAdmission) releaseOwnerLocked(owner string) {
	if owner == "" {
		return
	}
	n := a.owners[owner]
	if n <= 1 {
		delete(a.owners, owner)
		return
	}
	a.owners[owner] = n - 1
}

// rekeyOwner moves c's owner slot from oldOwner to newOwner and publishes
// c.setUploadOwner(newOwner) in the same critical section, so a concurrent
// releaseOwnerFor sees either the old or the new owner with its slot held,
// never a half-applied state. It returns false, changing nothing, when
// newOwner is at the cap or c is already torn down.
func (a *connAdmission) rekeyOwner(c *wsClient, oldOwner, newOwner string) bool {
	a.ownersMu.Lock()
	defer a.ownersMu.Unlock()
	// A torn-down connection must not reserve a fresh slot: its one-shot
	// release at unregister has already run and would never free it.
	select {
	case <-c.done:
		return false
	default:
	}
	a.releaseOwnerLocked(oldOwner)
	if !a.reserveOwnerLocked(newOwner) {
		// Re-claim the old slot so the eventual release stays balanced.
		a.reserveOwnerLocked(oldOwner)
		return false
	}
	c.setUploadOwner(newOwner)
	return true
}

// releaseOwnerFor releases the slot of c's CURRENT owner, read under the
// same lock rekeyOwner holds, so the two cannot interleave.
func (a *connAdmission) releaseOwnerFor(c *wsClient) {
	a.ownersMu.Lock()
	defer a.ownersMu.Unlock()
	a.releaseOwnerLocked(c.uploadOwnerKey())
}

// allowSend is the per-owner send ceiling, consulted after the per-conn
// limiter admits a send. The budget mirrors the per-conn shape (1/s, burst
// 5) so a single tab sees no change. Owner "" always admits.
func (a *connAdmission) allowSend(owner string) bool {
	if owner == "" {
		return true
	}
	if v, ok := a.sendLimiters.Load(owner); ok {
		return v.(*rate.Limiter).Allow()
	}
	// LoadOrStore returns the canonical limiter on a concurrent create.
	v, _ := a.sendLimiters.LoadOrStore(owner, rate.NewLimiter(rate.Every(time.Second), 5))
	return v.(*rate.Limiter).Allow()
}

// close drops the per-owner state. Shutdown calls it after every client
// goroutine has exited, so no reserve, release or send check is in flight.
func (a *connAdmission) close() {
	a.ownersMu.Lock()
	clear(a.owners)
	a.ownersMu.Unlock()
	a.sendLimiters.Clear()
}
