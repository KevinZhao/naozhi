package server

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// isAuthenticated checks auth without writing an error response. Used by
// endpoints that serve partial data to unauthenticated callers (e.g. /health).
func (s *Server) isAuthenticated(r *http.Request) bool {
	if s.dashboardToken == "" {
		return true
	}
	// Bearer header
	auth := r.Header.Get("Authorization")
	token := strings.TrimPrefix(auth, "Bearer ")
	if strings.HasPrefix(auth, "Bearer ") && subtle.ConstantTimeCompare([]byte(token), []byte(s.dashboardToken)) == 1 {
		return true
	}
	// Cookie fallback — value is HMAC-derived, not the raw token
	if c, err := r.Cookie(authCookieName); err == nil {
		expected := s.cookieMAC()
		return subtle.ConstantTimeCompare([]byte(c.Value), []byte(expected)) == 1
	}
	return false
}

// checkBearerAuth validates the dashboard API token. Returns true if authorized.
func (s *Server) checkBearerAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.isAuthenticated(r) {
		return true
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

func (s *Server) serveLoginPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := w.Write([]byte(loginPageHTML)); err != nil {
		slog.Debug("serve login page", "err", err)
	}
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	// Per-IP rate limiting to prevent single-attacker global lockout.
	// Uses RemoteAddr only — X-Forwarded-For is attacker-controlled and trivially spoofed.
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if !s.loginLimiterFor(ip).Allow() {
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
	if s.dashboardToken == "" || subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.dashboardToken)) != 1 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		if _, err := w.Write([]byte(`{"error":"invalid token"}`)); err != nil {
			slog.Debug("write auth response", "err", err)
		}
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    s.cookieMAC(), // HMAC-derived, not raw token
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge:   86400 * 30, // 30 days
	})
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(`{"status":"ok"}`)); err != nil {
		slog.Debug("write login response", "err", err)
	}
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge:   -1,
	})
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(`{"status":"ok"}`)); err != nil {
		slog.Debug("write logout response", "err", err)
	}
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
<form id="login-form" action="/dashboard" method="GET" autocomplete="on">
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
