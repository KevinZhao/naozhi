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
	// cookieGen is a per-construction generation marker mixed into the
	// CookieMAC HMAC input so a shared cookieSecret still yields a distinct
	// MAC per process.
	cookieGen string
	// cookieGenSeq is an atomic counter mixed into CookieMAC; bumping it
	// (RotateCookieGen) invalidates every outstanding auth cookie (#826).
	cookieGenSeq atomic.Uint64
	// loginLimiter is an O(1) LRU-backed per-IP limiter.
	loginLimiter *ratelimit.Limiter
	// wsUpgradeLimiter is a separate, looser bucket for WS upgrade attempts so
	// bursty reconnects cannot starve the same IP's login budget (or vice versa).
	wsUpgradeLimiter *ratelimit.Limiter
	// unauthDashLimiter throttles unauthenticated GET /dashboard so scanners
	// cannot fingerprint the deployment or burn CPU on the login-page render.
	unauthDashLimiter *ratelimit.Limiter
	TrustedProxy      bool // trust X-Forwarded-For for client IP extraction
}

const maxLoginLimiters = 10000

// authCookieMaxAgeSeconds is the nz_auth cookie lifetime. The MAC carries no
// per-session identity, so MaxAge is the only server-side bound on a stolen
// cookie; 1h keeps the replay window short while sliding renewal spares
// active tabs (#2074).
const authCookieMaxAgeSeconds = 3600 // 1 hour

// New constructs a Handlers with default limiters; the *Allow methods
// tolerate nil limiters for hand-rolled fixtures.
func New(dashboardToken string, cookieSecret []byte, cookieGen string, trustedProxy bool) *Handlers {
	// An empty gen would make the MAC fully determined by (token, secret), so a
	// captured cookie would authenticate against any future instance (#470).
	if cookieGen == "" {
		cookieGen = RandomCookieGen()
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

// NewLoginLimiter returns the tight per-IP limiter for HTTP /api/auth/login
// and the WS `auth` inner message (both directly test credentials).
func NewLoginLimiter() *ratelimit.Limiter {
	return ratelimit.New(ratelimit.Config{
		Rate:    rate.Every(12 * time.Second), // 5 attempts per minute
		Burst:   5,
		MaxKeys: maxLoginLimiters,
		TTL:     10 * time.Minute,
	})
}

// NewWSUpgradeLimiter returns the per-IP WS-upgrade limiter, intentionally
// looser than NewLoginLimiter because the upgrade itself checks no credentials.
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
// Separate from LoginAllow to prevent cross-endpoint lockouts.
func (a *Handlers) WSUpgradeAllow(ip string) bool {
	if ip == "" {
		ip = unknownIPKey
	}
	if a.wsUpgradeLimiter == nil {
		// nil limiter (fixture without one): keep upgrade gating open.
		return true
	}
	return a.wsUpgradeLimiter.Allow(ip)
}

// UnauthDashAllow reports whether the given IP is allowed one more
// unauthenticated GET /dashboard; a nil limiter allows.
func (a *Handlers) UnauthDashAllow(ip string) bool {
	if ip == "" {
		ip = unknownIPKey
	}
	if a.unauthDashLimiter == nil {
		return true
	}
	return a.unauthDashLimiter.Allow(ip)
}

// CookieMAC returns the HMAC-derived auth cookie value so the raw dashboard
// token never appears in cookies. Returns "" when DashboardToken is empty so
// no caller can accept a cookie that "matches" the empty MAC. Input is framed
// as `token || \x00 || cookieGen || \x00 || seq` so a malicious split cannot
// collide with a legitimate one; seq lets RotateCookieGen invalidate all
// outstanding cookies (#826).
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
// cookieGenSeq (atomic, goroutine-safe). Hot-reload handlers must call it
// whenever the dashboard token or another auth secret changes (#826).
func (a *Handlers) RotateCookieGen() {
	a.cookieGenSeq.Add(1)
}

// IsAuthenticated checks auth without writing an error response. Used by
// endpoints that serve partial data to unauthenticated callers (e.g. /health).
func (a *Handlers) IsAuthenticated(r *http.Request) bool {
	if a.DashboardToken == "" {
		return true
	}
	// Compare SHA-256 digests so length differences do not leak via the
	// short-circuit inside ConstantTimeCompare.
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		token := strings.TrimPrefix(auth, "Bearer ")
		got := sha256.Sum256([]byte(token))
		want := sha256.Sum256([]byte(a.DashboardToken))
		if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
			return true
		}
	}
	// Cookie fallback — HMAC-derived, not the raw token. Bail when expected
	// is empty (token="" path) so a forged "" cookie is never accepted.
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
// (SameOriginOK) so a cross-origin attacker on a sibling subdomain cannot ride
// a victim's auth cookie through a hidden `fetch(..., {credentials:'include'})`.
// Safe methods (GET/HEAD/OPTIONS) skip the gate so bookmarks and preflight
// still work; callers with no Origin / Referer (curl, server scripts) pass —
// they can't carry a browser's session cookies.
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
		// Sliding renewal: re-issue the cookie only when the request carried a
		// valid nz_auth cookie — Bearer / no-token callers must not be handed
		// a session cookie.
		if a.cookieRequestAuthenticated(r) {
			a.writeAuthCookie(w, r)
		}
		next(w, r)
	}
}

// cookieRequestAuthenticated reports whether the request is authenticated via
// a valid nz_auth cookie (not Bearer or no-token mode); scopes sliding renewal.
func (a *Handlers) cookieRequestAuthenticated(r *http.Request) bool {
	if a.DashboardToken == "" {
		return false
	}
	c, err := r.Cookie(AuthCookieName)
	if err != nil {
		return false
	}
	expected := a.CookieMAC()
	if expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(expected)) == 1
}

func (a *Handlers) ServeLoginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// CSP pins the SHA-256 hashes of the inline <script>/<style> blocks instead
	// of `unsafe-inline`; hashes are computed at package init so an edit to the
	// inline blocks breaks loudly rather than silently broadening the policy.
	w.Header().Set("Content-Security-Policy", loginPageCSP)
	// HSTS only over TLS: on plain HTTP (loopback / LAN) it would poison the
	// browser's HSTS cache and break future HTTP access on the same origin.
	if a.IsSecure(r) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
	if _, err := w.Write([]byte(loginPageHTML)); err != nil {
		slog.Debug("serve login page", "err", err)
	}
}

// loginPageCSP is the strict CSP served with the login page: inline blocks are
// allowlisted by SHA-256 hash; `unsafe-inline` would let any XSS on this origin
// exfiltrate the dashboard token field.
var loginPageCSP = buildLoginPageCSP()

// init panics when extraction yields zero matches for either tag: falling back
// to 'none' would block the page's own inline blocks at first request.
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
	return "default-src 'none'; script-src " + scriptSrc + "; style-src " + styleSrc + "; connect-src 'self'; frame-ancestors 'none'"
}

// Separate regexes per tag: a `</(?:script|style)>` alternation would let a
// `<script>…</style>` cross-closure match produce the wrong hash.
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

// clientIP extracts the client IP from the request, honouring TrustedProxy.
func (a *Handlers) clientIP(r *http.Request) string {
	return netutil.ClientIP(r, a.TrustedProxy)
}

// IsSecure returns true if the connection is over TLS. With TrustedProxy it
// also trusts X-Forwarded-Proto (ALB/CloudFront). That header may be a
// comma-separated chain; only the last hop (the proxy we trust) is
// authoritative, so a client-injected leading value is never honoured.
// Scheme tokens are case-insensitive per RFC 7239 §5.4 (EqualFold).
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
// `<form action="/api/auth/noscript" method="POST">`. Without it a non-JS
// submit would POST the form-encoded `token=…` to `/dashboard`, where future
// middleware could log it. Contract (#800): r.Body is never parsed, so the
// token never enters r.PostForm; respond 400 with a plain "JavaScript
// required" page.
func (a *Handlers) HandleLoginNoScript(w http.ResponseWriter, r *http.Request) {
	// Bound + drain the body so the connection can be reused but the token
	// bytes never enter a parsed map; the read error is deliberately ignored.
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Explicit 400 (not 405) so the browser shows our message.
	w.WriteHeader(http.StatusBadRequest)
	if _, err := w.Write([]byte(noScriptLoginHTML)); err != nil {
		slog.Debug("noscript login write", "err", err)
	}
}

// noScriptLoginHTML is the static response body for HandleLoginNoScript —
// no embedded token or request-derived input.
const noScriptLoginHTML = `<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<title>naozhi — JavaScript required</title>
<meta name="viewport" content="width=device-width,initial-scale=1,maximum-scale=1,user-scalable=no">
<style>body{background:#0a0a0a;color:#e0e0e0;font-family:-apple-system,sans-serif;padding:2rem;max-width:42rem;margin:0 auto}h1{font-size:1.25rem;margin-bottom:1rem}p{line-height:1.6;color:#ccc}a{color:#4a9eff}</style>
</head><body>
<h1>JavaScript required</h1>
<p>The naozhi dashboard requires JavaScript to sign in. Please enable JavaScript and reload the login page.</p>
<p><a href="/dashboard">Back to login</a></p>
</body></html>`

func (a *Handlers) HandleLogin(w http.ResponseWriter, r *http.Request) {
	// HandleLogin sits outside RequireAuth (it GRANTS auth), so apply the
	// same-origin gate manually; also catches misconfigured reverse proxies.
	if !SameOriginOK(r, a.TrustedProxy) {
		slog.Warn("rejecting cross-origin login attempt",
			"origin", osutil.SanitizeForLog(r.Header.Get("Origin"), 256),
			"host", osutil.SanitizeForLog(r.Host, 256))
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	// With TrustedProxy=true an XFF-less request would fall back to the proxy's
	// single IP, so every such caller would share one loginLimiter bucket that
	// one attacker can burn. A trusted proxy MUST stamp XFF; fail loud (#528).
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
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	// Same SHA-256 pre-digest as IsAuthenticated. "No token configured" is
	// combined with the compare result via bitwise AND (no `||` short-circuit)
	// so it is not remotely distinguishable by timing.
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
	a.writeAuthCookie(w, r)
	httputil.WriteOK(w)
}

// writeAuthCookie issues (or re-issues) the nz_auth cookie with a fresh
// MaxAge window (login + sliding renewal on authenticated requests).
func (a *Handlers) writeAuthCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     AuthCookieName,
		Value:    a.CookieMAC(), // HMAC-derived, not raw token
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   a.IsSecure(r),
		MaxAge:   authCookieMaxAgeSeconds,
	})
}

func (a *Handlers) HandleLogout(w http.ResponseWriter, r *http.Request) {
	// Logout MUST NOT call RotateCookieGen(): every browser holds the *same*
	// CookieMAC, so a global seq bump would let any cookie holder — including a
	// thief — log out every operator (#1913). Per-cookie revocation needs
	// per-session identity (#389).
	http.SetCookie(w, &http.Cookie{
		Name:     AuthCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   a.IsSecure(r),
		MaxAge:   -1,
	})
	// Also clear the nz_anon per-browser owner label (#2157). Field set mirrors
	// mintAnonCookie in internal/server/send_anon_cookie.go; the name is a
	// literal because this package cannot import internal/server.
	http.SetCookie(w, &http.Cookie{
		Name:     "nz_anon",
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
<meta name="viewport" content="width=device-width,initial-scale=1,maximum-scale=1,user-scalable=no">
<title>naozhi</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{background:#0a0a0a;color:#e0e0e0;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,monospace;display:flex;align-items:center;justify-content:center;min-height:100vh}
.login{background:#161616;border:1px solid #2a2a2a;border-radius:12px;padding:2.5rem;width:340px;text-align:center}
.login h1{font-size:1.5rem;margin-bottom:.3rem;font-weight:600;letter-spacing:-.02em}
.login p{color:#666;font-size:.85rem;margin-bottom:1.5rem}
input[type="text"]{position:absolute;left:-9999px;width:1px;height:1px}
label[for=token]{position:absolute;left:-9999px}
input[type="password"]{width:100%;padding:.75rem 1rem;background:#0a0a0a;border:1px solid #333;border-radius:8px;color:#e0e0e0;font-size:.95rem;outline:none;margin-bottom:1rem;transition:border-color .2s}
input[type="password"]:focus{border-color:#4a9eff}
button{width:100%;padding:.75rem;background:#4a9eff;color:#fff;border:none;border-radius:8px;font-size:.95rem;cursor:pointer;font-weight:500;transition:background .2s}
button:hover{background:#3a8eef}button:active{background:#2a7edf}
.error{color:#ef4444;font-size:.85rem;margin-top:.75rem;min-height:1.2em}
/* The login page renders before any dashboard JS, so it can't read the
   persisted nz_theme; follow the OS preference instead. Dark stays the
   default above; light users no longer get a jarring near-black card. */
@media (prefers-color-scheme:light){
  body{background:#f6f8fa;color:#1f2328}
  .login{background:#fff;border-color:#d0d7de;box-shadow:0 1px 3px rgba(27,31,36,.08)}
  .login p{color:#656d76}
  input[type="password"]{background:#fff;border-color:#d0d7de;color:#1f2328}
  input[type="password"]:focus{border-color:#0969da}
  button{background:#0969da}button:hover{background:#0860c9}button:active{background:#0757ba}
}
</style></head><body>
<div class="login">
<h1>naozhi</h1>
<p>enter token to continue</p>
<form id="login-form" action="/api/auth/noscript" method="POST" autocomplete="on">
<input type="text" name="username" autocomplete="username" value="naozhi" tabindex="-1" aria-hidden="true">
<label for="token">dashboard token</label>
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
    else{document.getElementById('err').textContent=res.status===429?'尝试过多，请稍后再试':'invalid token'}
  }catch(e){document.getElementById('err').textContent='network error'}
});
</script></body></html>`
