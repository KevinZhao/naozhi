package server

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

	"github.com/naozhi/naozhi/internal/ratelimit"
	"golang.org/x/time/rate"
)

// AuthHandlers provides authentication middleware and login/logout endpoints.
type AuthHandlers struct {
	dashboardToken string
	cookieSecret   []byte
	// cookieGen is an opaque per-construction generation marker mixed into
	// the cookieMAC HMAC input. Two roles:
	//
	//   1. Per-process restart rotation: cookieSecret is regenerated on
	//      each fresh stateDir, but operators that share stateDir across
	//      restarts (the common case) keep the same secret — meaning a
	//      pre-restart MAC would still verify after restart. Mixing
	//      cookieGen ensures every (process, secret, token) triple yields
	//      a distinct MAC even when the first two are stable.
	//
	//   2. Future hot-reload of dashboardToken: the rotation handler can
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
	// cookieGenSeq is an atomic counter mixed into cookieMAC alongside
	// cookieGen. Bumping it (via RotateCookieGen) immediately invalidates
	// every outstanding auth cookie because cookieMAC's HMAC input
	// changes — even at-rest browsers carrying the old cookie value will
	// fail constant-time compare on their next request and be
	// re-challenged at /api/auth/login.
	//
	// R245-SEC-2 (#826): closes the hot-rotate gap. Pre-fix, cookieGen
	// only changed at process restart, so a hot-reload of dashboardToken
	// (or of any other secret-rotation event handlers added later) left
	// existing cookies valid for the 24h MaxAge window. Per-process
	// restart rotation still works (cookieGen seed continues to vary by
	// time.Now().UnixNano() at construction); RotateCookieGen layers
	// in-process freshness without disturbing the seed-once contract.
	//
	// atomic.Uint64 is zero-value safe so existing struct-literal
	// fixtures (csrf_test.go, debug_pprof_test.go, etc.) keep working
	// without changes. Read on the cookieMAC hot path is a single
	// atomic load — same cost class as the existing cookieGen read.
	cookieGenSeq atomic.Uint64
	// loginLimiter is an O(1) LRU-backed per-IP limiter. At 10k attacking IPs
	// the previous two-pass O(n) scan was done under a single mutex and could
	// block legitimate logins; the ratelimit package does insertion, LRU
	// eviction and TTL reset in constant time.
	loginLimiter *ratelimit.Limiter
	// R191-SEC-M2: wsUpgradeLimiter is a separate bucket gated ONLY on WS
	// upgrade attempts. Previously the Hub used loginAllow directly, so 5
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
	trustedProxy      bool // trust X-Forwarded-For for client IP extraction
}

const maxLoginLimiters = 10000

// newLoginLimiter returns the per-IP rate limiter for HTTP /api/auth/login
// and for the WS `auth` inner message (both of which directly test
// credentials and deserve tight brute-force budgets).
func newLoginLimiter() *ratelimit.Limiter {
	return ratelimit.New(ratelimit.Config{
		Rate:    rate.Every(12 * time.Second), // 5 attempts per minute
		Burst:   5,
		MaxKeys: maxLoginLimiters,
		TTL:     10 * time.Minute,
	})
}

// newWSUpgradeLimiter returns the per-IP WS-upgrade limiter. It is
// intentionally looser than newLoginLimiter because the upgrade itself
// performs no credential check (cookie auth happens inline; password auth
// happens via the `auth` message which goes through loginLimiter).
func newWSUpgradeLimiter() *ratelimit.Limiter {
	return ratelimit.New(ratelimit.Config{
		Rate:    rate.Every(time.Second), // 60 attempts per minute sustained
		Burst:   20,                      // tolerate tab-reload / mobile-wake bursts
		MaxKeys: maxLoginLimiters,
		TTL:     10 * time.Minute,
	})
}

// loginAllow reports whether the given IP is allowed one more login attempt.
// Empty IPs share a single bucket so back-pressure is preserved when client
// IP resolution fails.
func (a *AuthHandlers) loginAllow(ip string) bool {
	if ip == "" {
		ip = unknownIPKey
	}
	return a.loginLimiter.Allow(ip)
}

// wsUpgradeAllow reports whether the given IP is allowed one more WS upgrade.
// Separate from loginAllow (R191-SEC-M2) to prevent WS-flood → login-DoS
// and login-flood → WS-DoS cross-endpoint lockouts.
func (a *AuthHandlers) wsUpgradeAllow(ip string) bool {
	if ip == "" {
		ip = unknownIPKey
	}
	if a.wsUpgradeLimiter == nil {
		// Fallback so tests that construct AuthHandlers without the new
		// limiter don't silently disable upgrade gating (return false would
		// break them; return true preserves prior behaviour).
		return true
	}
	return a.wsUpgradeLimiter.Allow(ip)
}

// unauthDashAllow reports whether the given IP is allowed one more
// unauthenticated GET /dashboard. Returns true when the limiter has not
// been wired (older test constructions) so test fixtures don't break.
// R230C-SEC-12.
func (a *AuthHandlers) unauthDashAllow(ip string) bool {
	if ip == "" {
		ip = unknownIPKey
	}
	if a.unauthDashLimiter == nil {
		return true
	}
	return a.unauthDashLimiter.Allow(ip)
}

// cookieMAC returns an HMAC-derived value used as the auth cookie value.
// This prevents the raw dashboard token from appearing in cookies.
//
// R245-SEC-9 [BREAKING-LOCAL]: returns "" when dashboardToken is empty
// rather than HMAC(secret, ""). The previous form computed a deterministic
// MAC over the empty string that any caller could replay; isAuthenticated
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
func (a *AuthHandlers) cookieMAC() string {
	if a.dashboardToken == "" {
		return ""
	}
	mac := hmac.New(sha256.New, a.cookieSecret)
	mac.Write([]byte(a.dashboardToken))
	mac.Write([]byte{0}) // domain separator: token || \x00 || cookieGen
	mac.Write([]byte(a.cookieGen))
	mac.Write([]byte{0}) // domain separator: cookieGen || \x00 || seq
	var seqBuf [20]byte
	mac.Write(strconv.AppendUint(seqBuf[:0], a.cookieGenSeq.Load(), 10))
	return hex.EncodeToString(mac.Sum(nil))
}

// RotateCookieGen invalidates every outstanding auth cookie by bumping
// the cookieGenSeq counter mixed into cookieMAC. Safe to call from any
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
func (a *AuthHandlers) RotateCookieGen() {
	a.cookieGenSeq.Add(1)
}

// isAuthenticated checks auth without writing an error response. Used by
// endpoints that serve partial data to unauthenticated callers (e.g. /health).
func (a *AuthHandlers) isAuthenticated(r *http.Request) bool {
	if a.dashboardToken == "" {
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
		want := sha256.Sum256([]byte(a.dashboardToken))
		if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
			return true
		}
	}
	// Cookie fallback — value is HMAC-derived, not the raw token.
	// R245-SEC-9: defence in depth — bail when expected is empty (token=""
	// path). The early-return at the top already covers the single-call
	// production path; this check ensures any future call site that
	// reorders the no-token short-circuit cannot accept a forged "" cookie.
	if c, err := r.Cookie(authCookieName); err == nil {
		expected := a.cookieMAC()
		if expected == "" {
			return false
		}
		return subtle.ConstantTimeCompare([]byte(c.Value), []byte(expected)) == 1
	}
	return false
}

// requireAuth is an HTTP middleware that rejects unauthenticated requests.
//
// State-changing methods additionally pass through a same-origin gate
// (sameOriginOK) so a cross-origin attacker on a sibling subdomain
// (evil.naozhi-host.example) cannot ride a victim's auth cookie through a
// hidden `fetch('...', {credentials:'include'})`. Safe methods (GET/HEAD/
// OPTIONS) skip the gate so bookmarks and preflight still work. The gate
// allows callers with no Origin / Referer header (curl, server scripts) —
// those can't carry a browser's session cookies. R31-SEC1 / R26-SEC1.
func (a *AuthHandlers) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isSafeMethod(r.Method) && !sameOriginOK(r, a.trustedProxy) {
			slog.Warn("rejecting cross-origin mutating request",
				"method", r.Method, "path", r.URL.Path,
				"origin", r.Header.Get("Origin"), "host", r.Host)
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if !a.isAuthenticated(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (a *AuthHandlers) serveLoginPage(w http.ResponseWriter, r *http.Request) {
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
	// Mirrors the isSecure gate used by handleLogin's cookie Secure flag.
	if a.isSecure(r) {
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
// Delegates to the package-level clientIP helper which handles trustedProxy.
func (a *AuthHandlers) clientIP(r *http.Request) string {
	return clientIP(r, a.trustedProxy)
}

// isSecure returns true if the connection is over TLS.
// When trustedProxy is enabled, also trusts the X-Forwarded-Proto header
// (set by ALB/CloudFront). Without trustedProxy, only trusts r.TLS.
func (a *AuthHandlers) isSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return a.trustedProxy && r.Header.Get("X-Forwarded-Proto") == "https"
}

// handleLoginNoScript is the form-action target for the login page's
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
func (a *AuthHandlers) handleLoginNoScript(w http.ResponseWriter, r *http.Request) {
	// Bound + drain the body so the connection can be reused but the
	// token bytes never enter a parsed map. MaxBytesReader caps at
	// 1 KiB — same ceiling handleLogin uses for its JSON body. The
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

// noScriptLoginHTML is the response body for handleLoginNoScript. Plain
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

func (a *AuthHandlers) handleLogin(w http.ResponseWriter, r *http.Request) {
	// handleLogin sits outside requireAuth (it's the endpoint that GRANTS
	// auth), so apply the same-origin gate manually. A cross-origin login
	// form post cannot be exploited for CSRF (attacker would need to know
	// the user's token), but still enforce for consistency and to catch
	// misconfigured reverse proxies before they send secrets around.
	// R31-SEC1 / R26-SEC1.
	if !sameOriginOK(r, a.trustedProxy) {
		slog.Warn("rejecting cross-origin login attempt",
			"origin", r.Header.Get("Origin"), "host", r.Host)
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	// R247-SEC-25 (#528): when trustedProxy=true and X-Forwarded-For is
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
	if !requestHasResolvableClientIP(r, a.trustedProxy) {
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
	if !a.loginAllow(ip) {
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
	if err := decodeJSONBody(r, &req); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	// Same SHA-256 pre-digest trick as isAuthenticated so a timing probe
	// cannot distinguish "wrong length" from "wrong bytes" — ConstantTimeCompare
	// short-circuits on length mismatch. Aligns both auth entry points.
	//
	// R220-SEC-2: keep the "no token configured" decision inside the same
	// branch as the constant-time compare result, AND combine via bitwise
	// AND of two int comparisons (no `||` short-circuit). Previous form
	// `if a.dashboardToken == "" || !matched` returned faster on empty
	// token because the compare-result branch was skipped, leaving a
	// remote-observable timing distinction between "no token" vs
	// "configured but wrong". The `byte(...)` widening forces both
	// operands to be evaluated regardless of the first comparison's
	// result.
	gotLogin := sha256.Sum256([]byte(req.Token))
	wantLogin := sha256.Sum256([]byte(a.dashboardToken))
	matched := subtle.ConstantTimeCompare(gotLogin[:], wantLogin[:])
	configured := 0
	if a.dashboardToken != "" {
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
		Name:     authCookieName,
		Value:    a.cookieMAC(), // HMAC-derived, not raw token
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   a.isSecure(r),
		MaxAge:   86400, // 1 day
	})
	writeOK(w)
}

func (a *AuthHandlers) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   a.isSecure(r),
		MaxAge:   -1,
	})
	writeOK(w)
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
