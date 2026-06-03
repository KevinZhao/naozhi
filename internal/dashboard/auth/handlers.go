package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cryptoutil"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/netutil"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/ratelimit"
	"golang.org/x/time/rate"
)

// Handlers provides authentication middleware and login/logout endpoints.
type Handlers struct {
	DashboardToken string
	cookieSecret   []byte
	// cookieGen is an opaque per-construction generation marker mixed into
	// the CookieMAC HMAC input. Two roles:
	//
	//   1. Per-process restart rotation: cookieSecret is regenerated on
	//      each fresh stateDir, but operators that share stateDir across
	//      restarts (the common case) keep the same secret — meaning a
	//      pre-restart MAC would still verify after restart. Mixing
	//      cookieGen ensures every (process, secret, token) triple yields
	//      a distinct MAC even when the first two are stable.
	//
	//   2. Future hot-reload of DashboardToken: the rotation handler can
	//      bump cookieGen to immediately invalidate every outstanding
	//      cookie without rotating cookieSecret (which would also kick
	//      every other authenticated surface). Today this field is set
	//      once at construction; the bump path is left for the eventual
	//      rotation RFC.
	//
	// R247-SEC-17 / R245-SEC-2 / R243-SEC-13 / R242-SEC-5 — same root
	// cause across four review rounds: HMAC(secret, token) lacked any
	// freshness input.
	cookieGen string
	// cookieGenSeq is an atomic counter mixed into CookieMAC alongside
	// cookieGen. Bumping it (via RotateCookieGen) immediately invalidates
	// every outstanding auth cookie because CookieMAC's HMAC input
	// changes — even at-rest browsers carrying the old cookie value will
	// fail constant-time compare on their next request and be
	// re-challenged at /api/auth/login.
	//
	// R245-SEC-2 (#826): closes the hot-rotate gap. Pre-fix, cookieGen
	// only changed at process restart, so a hot-reload of DashboardToken
	// (or of any other secret-rotation event handlers added later) left
	// existing cookies valid for the 24h MaxAge window. Per-process
	// restart rotation still works (cookieGen seed continues to vary by
	// time.Now().UnixNano() at construction); RotateCookieGen layers
	// in-process freshness without disturbing the seed-once contract.
	//
	// atomic.Uint64 is zero-value safe so existing struct-literal
	// fixtures (csrf_test.go, debug_pprof_test.go, etc.) keep working
	// without changes. Read on the CookieMAC hot path is a single
	// atomic load — same cost class as the existing cookieGen read.
	cookieGenSeq atomic.Uint64
	// loginLimiter is an O(1) LRU-backed per-IP limiter. At 10k attacking IPs
	// the previous two-pass O(n) scan was done under a single mutex and could
	// block legitimate logins; the ratelimit package does insertion, LRU
	// eviction and TTL reset in constant time.
	loginLimiter *ratelimit.Limiter
	// R191-SEC-M2: wsUpgradeLimiter is a separate bucket gated ONLY on WS
	// upgrade attempts. Previously the Hub used LoginAllow directly, so 5
	// rapid WS connects from a NATed client could starve the same IP's HTTP
	// login attempts for 60s (and vice versa). The upgrade path sees
	// legitimately bursty traffic on tab-reload / mobile wake, so we grant
	// a looser budget; the inner /api/auth/login POST still uses the tight
	// loginLimiter for brute-force guard.
	wsUpgradeLimiter *ratelimit.Limiter
	// R230C-SEC-12: unauthDashLimiter throttles unauthenticated GET /dashboard.
	// Unauthenticated users would otherwise hit the login template renderer
	// (and the login-page CSP/HTML asset) without any back-pressure, which a
	// scanner can use both to fingerprint the deployment and to push CPU on
	// the embed.FS read + crypto/rand cookie path. Same bucket family as
	// wsUpgradeLimiter (60/min sustained, 20 burst) is plenty for a real
	// human refreshing the login page; sustained scanners drop to 429.
	unauthDashLimiter *ratelimit.Limiter
	TrustedProxy      bool // trust X-Forwarded-For for client IP extraction
}

const maxLoginLimiters = 10000

// New constructs a Handlers from the auth-relevant ServerOptions inputs.
// Phase 3a (server-split-phase4-design.md §6.5 Plan B): exported ctor so
// internal/server's buildAuthHandlers can construct without needing access
// to unexported fields. Limiters wired with sane defaults; LoginAllow /
// WSUpgradeAllow / UnauthDashAllow handle nil limiter fallback for legacy
// hand-rolled fixtures.
func New(dashboardToken string, cookieSecret []byte, cookieGen string, trustedProxy bool) *Handlers {
	// R241-SEC-10 (#470): the cookie MAC is HMAC(secret, token || gen || seq).
	// When the caller passes an empty gen the MAC collapses to a value fully
	// determined by (token, secret) alone — deterministic across processes,
	// so a captured cookie keeps authenticating against any future instance
	// sharing the same token + secret. Seed an unpredictable per-construction
	// gen so the no-seed path still rotates the MAC on every restart instead
	// of replaying a fixed value. Callers that supply a gen (production seeds
	// one from server.go) keep their explicit value.
	if cookieGen == "" {
		cookieGen = cryptoutil.RandomCookieGen()
	}
	return &Handlers{
		DashboardToken:    dashboardToken,
		cookieSecret:      cookieSecret,
		cookieGen:         cookieGen,
		loginLimiter:      NewLoginLimiter(),
		wsUpgradeLimiter:  NewWSUpgradeLimiter(),
		unauthDashLimiter: NewWSUpgradeLimiter(),
		TrustedProxy:      trustedProxy,
	}
}

// NewLoginLimiter returns the per-IP rate limiter for HTTP /api/auth/login
// and for the WS `auth` inner message (both of which directly test
// credentials and deserve tight brute-force budgets).
func NewLoginLimiter() *ratelimit.Limiter {
	return ratelimit.New(ratelimit.Config{
		Rate:    rate.Every(12 * time.Second), // 5 attempts per minute
		Burst:   5,
		MaxKeys: maxLoginLimiters,
		TTL:     10 * time.Minute,
	})
}

// NewWSUpgradeLimiter returns the per-IP WS-upgrade limiter. It is
// intentionally looser than NewLoginLimiter because the upgrade itself
// performs no credential check (cookie auth happens inline; password auth
// happens via the `auth` message which goes through loginLimiter).
func NewWSUpgradeLimiter() *ratelimit.Limiter {
	return ratelimit.New(ratelimit.Config{
		Rate:    rate.Every(time.Second), // 60 attempts per minute sustained
		Burst:   20,                      // tolerate tab-reload / mobile-wake bursts
		MaxKeys: maxLoginLimiters,
		TTL:     10 * time.Minute,
	})
}

// LoginAllow reports whether the given IP is allowed one more login attempt.
// Empty IPs share a single bucket so back-pressure is preserved when client
// IP resolution fails.
func (a *Handlers) LoginAllow(ip string) bool {
	if ip == "" {
		ip = unknownIPKey
	}
	return a.loginLimiter.Allow(ip)
}

// WSUpgradeAllow reports whether the given IP is allowed one more WS upgrade.
// Separate from LoginAllow (R191-SEC-M2) to prevent WS-flood → login-DoS
// and login-flood → WS-DoS cross-endpoint lockouts.
func (a *Handlers) WSUpgradeAllow(ip string) bool {
	if ip == "" {
		ip = unknownIPKey
	}
	if a.wsUpgradeLimiter == nil {
		// Fallback so tests that construct Handlers without the new
		// limiter don't silently disable upgrade gating (return false would
		// break them; return true preserves prior behaviour).
		return true
	}
	return a.wsUpgradeLimiter.Allow(ip)
}

// UnauthDashAllow reports whether the given IP is allowed one more
// unauthenticated GET /dashboard. Returns true when the limiter has not
// been wired (older test constructions) so test fixtures don't break.
// R230C-SEC-12.
func (a *Handlers) UnauthDashAllow(ip string) bool {
	if ip == "" {
		ip = unknownIPKey
	}
	if a.unauthDashLimiter == nil {
		return true
	}
	return a.unauthDashLimiter.Allow(ip)
}

// CookieMAC returns an HMAC-derived value used as the auth cookie value.
// This prevents the raw dashboard token from appearing in cookies.
//
// R245-SEC-9 [BREAKING-LOCAL]: returns "" when DashboardToken is empty
// rather than HMAC(secret, ""). The previous form computed a deterministic
// MAC over the empty string that any caller could replay; IsAuthenticated
// already short-circuits to true on empty token so the value was unused
// today, but the residual MAC was a regression-bait. Returning "" makes
// the no-token contract explicit at the source so future callers cannot
// accidentally accept a cookie value that "matches" the empty MAC.
//
// R247-SEC-17: HMAC input now includes cookieGen so the MAC rotates on
// every process restart (and any future hot-reload that bumps cookieGen)
// even when stateDir / cookieSecret are stable. The serialised input uses
// a length-prefixed framing (`token || \x00 || cookieGen || \x00 || seq`)
// so a malicious (token, gen, seq) split that concatenates to the same
// bytes cannot collide with a legitimate split.
//
// R245-SEC-2 (#826): cookieGenSeq is also mixed in so RotateCookieGen
// produces a new MAC immediately, invalidating every outstanding cookie
// without needing a process restart. The atomic load is the same cost
// class as the prior plain-string read.
func (a *Handlers) CookieMAC() string {
	if a.DashboardToken == "" {
		return ""
	}
	mac := hmac.New(sha256.New, a.cookieSecret)
	mac.Write([]byte(a.DashboardToken))
	mac.Write([]byte{0}) // domain separator: token || \x00 || cookieGen
	mac.Write([]byte(a.cookieGen))
	mac.Write([]byte{0}) // domain separator: cookieGen || \x00 || seq
	var seqBuf [20]byte
	mac.Write(strconv.AppendUint(seqBuf[:0], a.cookieGenSeq.Load(), 10))
	return hex.EncodeToString(mac.Sum(nil))
}

// RotateCookieGen invalidates every outstanding auth cookie by bumping
// the cookieGenSeq counter mixed into CookieMAC. Safe to call from any
// goroutine — uses an atomic increment with no lock.
//
// R245-SEC-2 (#826): the rotation hook a future hot-reload handler must
// invoke whenever the dashboard token (or any other auth-relevant
// secret) changes. Without this call, a token rotation at runtime
// leaves the prior token's cookies valid for the full 24h MaxAge
// because cookieGen was only seeded once at construction. Calling
// RotateCookieGen on the rotation event closes that window — every
// browser carrying an old MAC fails the constant-time compare on its
// next request and is sent back through /api/auth/login.
func (a *Handlers) RotateCookieGen() {
	a.cookieGenSeq.Add(1)
}

// IsAuthenticated checks auth without writing an error response. Used by
// endpoints that serve partial data to unauthenticated callers (e.g. /health).
func (a *Handlers) IsAuthenticated(r *http.Request) bool {
	if a.DashboardToken == "" {
		return true
	}
	// Bearer header. Compare SHA-256 digests so length differences do not
	// leak via the short-circuit branch inside ConstantTimeCompare (which
	// returns 0 immediately when operand lengths differ). Mirrors the
	// feishu webhook constantTimeEqualString pattern.
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		token := strings.TrimPrefix(auth, "Bearer ")
		got := sha256.Sum256([]byte(token))
		want := sha256.Sum256([]byte(a.DashboardToken))
		if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
			return true
		}
	}
	// Cookie fallback — value is HMAC-derived, not the raw token.
	// R245-SEC-9: defence in depth — bail when expected is empty (token=""
	// path). The early-return at the top already covers the single-call
	// production path; this check ensures any future call site that
	// reorders the no-token short-circuit cannot accept a forged "" cookie.
	if c, err := r.Cookie(AuthCookieName); err == nil {
		expected := a.CookieMAC()
		if expected == "" {
			return false
		}
		return subtle.ConstantTimeCompare([]byte(c.Value), []byte(expected)) == 1
	}
	return false
}

// RequireAuth is an HTTP middleware that rejects unauthenticated requests.
//
// State-changing methods additionally pass through a same-origin gate
// (SameOriginOK) so a cross-origin attacker on a sibling subdomain
// (evil.naozhi-host.example) cannot ride a victim's auth cookie through a
// hidden `fetch('...', {credentials:'include'})`. Safe methods (GET/HEAD/
// OPTIONS) skip the gate so bookmarks and preflight still work. The gate
// allows callers with no Origin / Referer header (curl, server scripts) —
// those can't carry a browser's session cookies. R31-SEC1 / R26-SEC1.
func (a *Handlers) RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !IsSafeMethod(r.Method) && !SameOriginOK(r, a.TrustedProxy) {
			slog.Warn("rejecting cross-origin mutating request",
				"method", r.Method, "path", osutil.SanitizeForLog(r.URL.Path, 256),
				"origin", osutil.SanitizeForLog(r.Header.Get("Origin"), 256),
				"host", osutil.SanitizeForLog(r.Host, 256))
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if !a.IsAuthenticated(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (a *Handlers) ServeLoginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// CSP uses hash-based allowlist for the single inline <script>/<style>
	// blocks baked into loginPageHTML. `unsafe-inline` would neutralise any
	// future XSS defence on this origin, so we pin the exact bytes of the
	// inline content instead. The hashes are computed once at package init
	// (see loginPageCSP) so the page stays static but any accidental edit
	// to the inline blocks immediately breaks loading — that's the
	// intended self-check: if the hash no longer matches, an operator
	// notices during manual review, rather than silently broadening the
	// policy.
	w.Header().Set("Content-Security-Policy", loginPageCSP)
	// R241-SEC-1: only send HSTS on TLS connections. Sending it over plain
	// HTTP (loopback or LAN deployments) pollutes the browser's HSTS cache
	// for 31536000 s and breaks future HTTP access on the same origin.
	// Mirrors the IsSecure gate used by HandleLogin's cookie Secure flag.
	if a.IsSecure(r) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
	if _, err := w.Write([]byte(loginPageHTML)); err != nil {
		slog.Debug("serve login page", "err", err)
	}
}

// loginPageCSP is the strict CSP served with the login page. The inline
// <script> and <style> blocks in loginPageHTML are allowlisted by their
// SHA-256 hashes; adding `unsafe-inline` (as the prior implementation did)
// would make any XSS on this origin capable of exfiltrating the dashboard
// token field. Hashes are extracted from loginPageHTML at package init so
// the string stays authoritative for both page bytes and CSP.
var loginPageCSP = buildLoginPageCSP()

// init enforces R232-SEC-9: extract failures (zero matches for either tag)
// would silently fall back to 'none', which serves the page with a CSP that
// blocks its own inline <script>/<style> — a "login broken" surface that
// only manifests at first request. Panic at package init so the regression
// is caught at process start.
func init() {
	scripts := extractInlineBlocks(loginPageHTML, inlineScriptRe)
	styles := extractInlineBlocks(loginPageHTML, inlineStyleRe)
	if len(scripts) == 0 || len(styles) == 0 {
		panic(fmt.Sprintf("dashboard_auth: loginPageCSP self-test failed: scripts=%d styles=%d (regex drift in loginPageHTML)",
			len(scripts), len(styles)))
	}
}

func buildLoginPageCSP() string {
	var scriptHashes, styleHashes []string
	for _, b := range extractInlineBlocks(loginPageHTML, inlineScriptRe) {
		scriptHashes = append(scriptHashes, "'sha256-"+hashInline(b)+"'")
	}
	for _, b := range extractInlineBlocks(loginPageHTML, inlineStyleRe) {
		styleHashes = append(styleHashes, "'sha256-"+hashInline(b)+"'")
	}
	scriptSrc := "'none'"
	if len(scriptHashes) > 0 {
		scriptSrc = strings.Join(scriptHashes, " ")
	}
	styleSrc := "'none'"
	if len(styleHashes) > 0 {
		styleSrc = strings.Join(styleHashes, " ")
	}
	return "default-src 'none'; script-src " + scriptSrc + "; style-src " + styleSrc + "; connect-src 'self'"
}

// Separate regexes per tag: a single `</(?:script|style)>` alternation would
// let a `<script>…</style>` cross-closure match and silently produce the
// wrong hash (CSP still refuses the page, failing closed, but this keeps
// the error surface obvious).
var (
	inlineScriptRe = regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`)
	inlineStyleRe  = regexp.MustCompile(`(?s)<style[^>]*>(.*?)</style>`)
)

func extractInlineBlocks(html string, re *regexp.Regexp) []string {
	matches := re.FindAllStringSubmatch(html, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

func hashInline(body string) string {
	sum := sha256.Sum256([]byte(body))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// clientIP extracts the client IP from the request.
// Delegates to the package-level clientIP helper which handles TrustedProxy.
func (a *Handlers) clientIP(r *http.Request) string {
	return netutil.ClientIP(r, a.TrustedProxy)
}

// IsSecure returns true if the connection is over TLS.
// When TrustedProxy is enabled, also trusts the X-Forwarded-Proto header
// (set by ALB/CloudFront). Without TrustedProxy, only trusts r.TLS.
//
// X-Forwarded-Proto may be a comma-separated chain (proto1, proto2) when
// multiple proxies prepend their own value; only the last hop (the proxy
// directly in front of naozhi, which we trust via TrustedProxy) is
// authoritative. A client-injected leading value must never be honoured,
// so we take the final segment. Per RFC 7239 §5.4 the scheme token is
// case-insensitive (Nginx may emit "HTTPS"), so compare with EqualFold.
func (a *Handlers) IsSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if !a.TrustedProxy {
		return false
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if i := strings.LastIndexByte(proto, ','); i >= 0 {
		proto = proto[i+1:]
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// HandleLoginNoScript is the form-action target for the login page's
// `<form action="/api/auth/noscript" method="POST">`. The login flow is
// fully JavaScript-driven (the JS submit handler intercepts and POSTs
// JSON to /api/auth/login); a non-JS browser that hits Submit would
// otherwise cause the form-encoded `token=…` body to be POSTed to
// `/dashboard` and land in server access logs as part of the URL-decoded
// body if any future middleware reads it.
//
// R243-SEC-15 (#800): defence-in-depth. Today no handler reads the form
// body, but the browser still ships the token inside an unencrypted POST
// frame (no TLS guarantee) and the proxy hop chain can log it. Routing
// the no-JS path to this dedicated handler makes the contract explicit:
// (a) we never read r.Body, so the token never enters server memory in
// log-format; (b) we return a 400 with a clear "JavaScript required"
// page so the operator knows what happened; (c) the response is plain
// HTML so a screen-reader user can still see the failure mode.
//
// We do NOT parse the form body — the io.Copy(io.Discard, ...) on a
// MaxBytesReader-bounded body drains and drops it without exposing it
// to slog. ParseForm would otherwise stash key/value pairs in
// r.PostForm where any debug handler could later dump them.
func (a *Handlers) HandleLoginNoScript(w http.ResponseWriter, r *http.Request) {
	// Bound + drain the body so the connection can be reused but the
	// token bytes never enter a parsed map. MaxBytesReader caps at
	// 1 KiB — same ceiling HandleLogin uses for its JSON body. The
	// drain is best-effort; closing without reading would also work
	// but some proxies hold the request open expecting the body to be
	// consumed. We deliberately ignore the read err — we already chose
	// to discard the bytes regardless of content.
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Explicit 400 (not 405) so the operator's browser shows our
	// message instead of the default ServeMux "Method Not Allowed".
	w.WriteHeader(http.StatusBadRequest)
	if _, err := w.Write([]byte(noScriptLoginHTML)); err != nil {
		slog.Debug("noscript login write", "err", err)
	}
}

// noScriptLoginHTML is the response body for HandleLoginNoScript. Plain
// static HTML — no embedded token, no embedded URL parameter, nothing
// derivable from request input. Kept short to fit a single TCP frame.
const noScriptLoginHTML = `<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<title>naozhi — JavaScript required</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>body{background:#0a0a0a;color:#e0e0e0;font-family:-apple-system,sans-serif;padding:2rem;max-width:42rem;margin:0 auto}h1{font-size:1.25rem;margin-bottom:1rem}p{line-height:1.6;color:#ccc}a{color:#4a9eff}</style>
</head><body>
<h1>JavaScript required</h1>
<p>The naozhi dashboard requires JavaScript to sign in. Please enable JavaScript and reload the login page.</p>
<p><a href="/dashboard">Back to login</a></p>
</body></html>`

func (a *Handlers) HandleLogin(w http.ResponseWriter, r *http.Request) {
	// HandleLogin sits outside RequireAuth (it's the endpoint that GRANTS
	// auth), so apply the same-origin gate manually. A cross-origin login
	// form post cannot be exploited for CSRF (attacker would need to know
	// the user's token), but still enforce for consistency and to catch
	// misconfigured reverse proxies before they send secrets around.
	// R31-SEC1 / R26-SEC1.
	if !SameOriginOK(r, a.TrustedProxy) {
		slog.Warn("rejecting cross-origin login attempt",
			"origin", osutil.SanitizeForLog(r.Header.Get("Origin"), 256),
			"host", osutil.SanitizeForLog(r.Host, 256))
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	// R247-SEC-25 (#528): when TrustedProxy=true and X-Forwarded-For is
	// missing or unparseable, a.clientIP(r) silently falls back to
	// r.RemoteAddr — which under ALB/CloudFront is the proxy's single IP.
	// Every legitimate XFF-less request would then share that one bucket,
	// letting a single attacker burn the loginLimiter slot for every other
	// XFF-less caller. In a properly configured trusted-proxy deployment
	// the proxy MUST stamp XFF, so an XFF-less request is either a proxy
	// misconfig or an attacker bypassing the proxy — either way, fail
	// loud (400) so the operator notices, instead of degrading to a
	// shared-bucket rate limit. Mirrors the AllowRequest gate on the
	// general HTTP rate-limiter (R244-SEC-P3-3).
	if !requestHasResolvableClientIP(r, a.TrustedProxy) {
		slog.Warn("login refused: trusted-proxy mode but X-Forwarded-For missing/unparseable",
			"remote", r.RemoteAddr, "xff", r.Header.Get("X-Forwarded-For"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if _, err := w.Write([]byte(`{"error":"missing X-Forwarded-For header"}`)); err != nil {
			slog.Debug("write XFF error response", "err", err)
		}
		return
	}
	ip := a.clientIP(r)
	if !a.LoginAllow(ip) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		if _, err := w.Write([]byte(`{"error":"too many attempts, try again later"}`)); err != nil {
			slog.Debug("write rate limit response", "err", err)
		}
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req struct {
		Token string `json:"token"`
	}
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	// Same SHA-256 pre-digest trick as IsAuthenticated so a timing probe
	// cannot distinguish "wrong length" from "wrong bytes" — ConstantTimeCompare
	// short-circuits on length mismatch. Aligns both auth entry points.
	//
	// R220-SEC-2: keep the "no token configured" decision inside the same
	// branch as the constant-time compare result, AND combine via bitwise
	// AND of two int comparisons (no `||` short-circuit). Previous form
	// `if a.DashboardToken == "" || !matched` returned faster on empty
	// token because the compare-result branch was skipped, leaving a
	// remote-observable timing distinction between "no token" vs
	// "configured but wrong". The `byte(...)` widening forces both
	// operands to be evaluated regardless of the first comparison's
	// result.
	gotLogin := sha256.Sum256([]byte(req.Token))
	wantLogin := sha256.Sum256([]byte(a.DashboardToken))
	matched := subtle.ConstantTimeCompare(gotLogin[:], wantLogin[:])
	configured := 0
	if a.DashboardToken != "" {
		configured = 1
	}
	if matched&configured == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		if _, err := w.Write([]byte(`{"error":"invalid token"}`)); err != nil {
			slog.Debug("write auth response", "err", err)
		}
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     AuthCookieName,
		Value:    a.CookieMAC(), // HMAC-derived, not raw token
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   a.IsSecure(r),
		MaxAge:   86400, // 1 day
	})
	httputil.WriteOK(w)
}

func (a *Handlers) HandleLogout(w http.ResponseWriter, r *http.Request) {
	// S9 (#389): clearing the browser cookie alone left the underlying MAC
	// valid for the full 24h MaxAge — a stolen cookie replayed freely after
	// logout. Bump cookieGenSeq so the issued MAC no longer authenticates;
	// IsAuthenticated's constant-time compare now fails for any outstanding
	// cookie, making logout a real server-side revocation.
	a.RotateCookieGen()
	http.SetCookie(w, &http.Cookie{
		Name:     AuthCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   a.IsSecure(r),
		MaxAge:   -1,
	})
	httputil.WriteOK(w)
}

const loginPageHTML = `<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>naozhi</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{background:#0a0a0a;color:#e0e0e0;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,monospace;display:flex;align-items:center;justify-content:center;min-height:100vh}
.login{background:#161616;border:1px solid #2a2a2a;border-radius:12px;padding:2.5rem;width:340px;text-align:center}
.login h1{font-size:1.5rem;margin-bottom:.3rem;font-weight:600;letter-spacing:-.02em}
.login p{color:#666;font-size:.85rem;margin-bottom:1.5rem}
input[type="text"]{position:absolute;left:-9999px;width:1px;height:1px}
input[type="password"]{width:100%;padding:.75rem 1rem;background:#0a0a0a;border:1px solid #333;border-radius:8px;color:#e0e0e0;font-size:.95rem;outline:none;margin-bottom:1rem;transition:border-color .2s}
input[type="password"]:focus{border-color:#4a9eff}
button{width:100%;padding:.75rem;background:#4a9eff;color:#fff;border:none;border-radius:8px;font-size:.95rem;cursor:pointer;font-weight:500;transition:background .2s}
button:hover{background:#3a8eef}button:active{background:#2a7edf}
.error{color:#ef4444;font-size:.85rem;margin-top:.75rem;min-height:1.2em}
</style></head><body>
<div class="login">
<h1>naozhi</h1>
<p>enter token to continue</p>
<form id="login-form" action="/api/auth/noscript" method="POST" autocomplete="on">
<input type="text" name="username" autocomplete="username" value="naozhi" tabindex="-1" aria-hidden="true">
<label for="token" style="position:absolute;left:-9999px">dashboard token</label>
<input type="password" name="token" id="token" autocomplete="current-password" placeholder="dashboard token" aria-label="dashboard token" autofocus>
<button type="submit" aria-label="Sign in">login</button>
</form>
<div class="error" id="err"></div>
</div>
<script>
document.getElementById('login-form').addEventListener('submit', async function(e){
  e.preventDefault();
  var t=document.getElementById('token').value.trim();
  if(!t)return;
  document.getElementById('err').textContent='';
  try{
    var res=await fetch('/api/auth/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({token:t})});
    if(res.ok){window.location.href='/dashboard'}
    else{document.getElementById('err').textContent='invalid token'}
  }catch(e){document.getElementById('err').textContent='network error'}
});
</script></body></html>`
