package server

import (
	"net/http"
	"net/url"
	"strings"
)

// isSafeMethod reports whether the HTTP method is safe per RFC 7231 §4.2.1
// — i.e. is expected to have no state-changing side effects. The CSRF
// Origin gate only applies to mutating methods so that GET prefetches,
// HEAD probes, and CORS preflight OPTIONS from any origin keep working.
func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// requestHost returns the effective host the browser (or trusted proxy)
// addressed this request at. Mirrors the wshub CheckOrigin helper: when
// trustedProxy is set and the X-Forwarded-Host header is present, we pick
// the first value from the (possibly comma-separated) list as RFC 7239
// allows. Otherwise we trust r.Host directly.
func requestHost(r *http.Request, trustedProxy bool) string {
	host := r.Host
	if trustedProxy {
		if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
			host = strings.TrimSpace(strings.SplitN(fwd, ",", 2)[0])
		}
	}
	return host
}

// sameOriginOK reports whether the Origin (or Referer fallback) header
// identifies the same host naozhi is serving on. Missing Origin AND
// Referer is treated as "not a browser navigation" (curl, server-to-server
// scripts) and passes — those callers don't suffer CSRF in the first place
// because they don't carry session cookies from a victim's browser.
//
// The caller must restrict this check to state-changing methods (see
// isSafeMethod); applying it to GET would break direct-URL navigation
// from bookmarks and external links where browsers routinely omit Origin.
//
// R31-SEC1 / R26-SEC1 / R58-SEC-001 / R60-SEC-001 defense-in-depth:
// SameSite=Strict cookies do not protect against same-registrable-domain
// cross-origin attackers (evil.naozhi-host.example → naozhi-host.example)
// — this gate closes that gap at the HTTP layer.
func sameOriginOK(r *http.Request, trustedProxy bool) bool {
	host := requestHost(r, trustedProxy)
	if host == "" {
		// Defensive: unknown Host means we can't validate Origin against
		// anything. Refuse the write to fail closed.
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Fall back to Referer. Some browsers omit Origin on same-origin
		// POSTs in legacy modes; Referer is still sent. If both are
		// missing the caller is probably a non-browser client.
		ref := r.Header.Get("Referer")
		if ref == "" {
			return true
		}
		u, err := url.Parse(ref)
		if err != nil || u.Host == "" {
			return false
		}
		// R191-SEC-M1: Reject non-http(s) Referer schemes. javascript:,
		// data:, ftp:, file:, blob:, etc. can parse with the correct host
		// but must not count as browser same-origin; a crafted non-browser
		// client (or misconfigured intermediary) could otherwise bypass
		// the CSRF gate by supplying ftp://host/x.
		if u.Scheme != "http" && u.Scheme != "https" {
			return false
		}
		return u.Host == host
	}
	// RFC 6454 allows "null" for opaque origins (sandboxed iframes, file://
	// pages). Treat it as a definite cross-origin — never same-origin with
	// our Host.
	if origin == "null" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	// R191-SEC-M1: Same scheme guard as the Referer fallback above.
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host == host
}
