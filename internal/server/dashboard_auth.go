package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/ratelimit"
	"golang.org/x/time/rate"
)

// AuthHandlers provides authentication middleware and login/logout endpoints.
type AuthHandlers struct {
	dashboardToken string
	cookieSecret   []byte
	// loginLimiter is an O(1) LRU-backed per-IP limiter. At 10k attacking IPs
	// the previous two-pass O(n) scan was done under a single mutex and could
	// block legitimate logins; the ratelimit package does insertion, LRU
	// eviction and TTL reset in constant time.
	loginLimiter *ratelimit.Limiter
	trustedProxy bool // trust X-Forwarded-For for client IP extraction
}

const maxLoginLimiters = 10000

// newLoginLimiter returns the shared per-IP login rate limiter used by both
// the HTTP /api/auth/login endpoint and WebSocket auth.
func newLoginLimiter() *ratelimit.Limiter {
	return ratelimit.New(ratelimit.Config{
		Rate:    rate.Every(12 * time.Second), // 5 attempts per minute
		Burst:   5,
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

// cookieMAC returns an HMAC-derived value used as the auth cookie value.
// This prevents the raw dashboard token from appearing in cookies.
func (a *AuthHandlers) cookieMAC() string {
	mac := hmac.New(sha256.New, a.cookieSecret)
	mac.Write([]byte(a.dashboardToken))
	return hex.EncodeToString(mac.Sum(nil))
}

// isAuthenticated checks auth without writing an error response. Used by
// endpoints that serve partial data to unauthenticated callers (e.g. /health).
func (a *AuthHandlers) isAuthenticated(r *http.Request) bool {
	if a.dashboardToken == "" {
		return true
	}
	// Bearer header
	auth := r.Header.Get("Authorization")
	token := strings.TrimPrefix(auth, "Bearer ")
	if strings.HasPrefix(auth, "Bearer ") && subtle.ConstantTimeCompare([]byte(token), []byte(a.dashboardToken)) == 1 {
		return true
	}
	// Cookie fallback — value is HMAC-derived, not the raw token
	if c, err := r.Cookie(authCookieName); err == nil {
		expected := a.cookieMAC()
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

func (a *AuthHandlers) serveLoginPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
	w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	if _, err := w.Write([]byte(loginPageHTML)); err != nil {
		slog.Debug("serve login page", "err", err)
	}
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	if a.dashboardToken == "" || subtle.ConstantTimeCompare([]byte(req.Token), []byte(a.dashboardToken)) != 1 {
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
<form id="login-form" action="/dashboard" method="POST" autocomplete="on">
<input type="text" name="username" autocomplete="username" value="naozhi" tabindex="-1" aria-hidden="true">
<input type="password" name="token" id="token" autocomplete="current-password" placeholder="dashboard token" autofocus>
<button type="submit">login</button>
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
