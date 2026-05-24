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

// requestScheme returns the effective scheme ("http" or "https") the browser
// (or trusted proxy) used to reach naozhi. Mirrors AuthHandlers.isSecure:
// r.TLS is the source of truth for direct HTTPS, and X-Forwarded-Proto is
// honored only when trustedProxy is set (ALB / CloudFront / nginx in front).
//
// [R247-SEC-1] Used by sameOriginOK to require Origin/Referer scheme to
// match the request scheme: an HTTPS dashboard request must not be paired
// with an `Origin: http://...` header (mixed-content downgrade) and vice
// versa, otherwise an attacker on a sibling http:// origin can satisfy the
// host-only same-origin gate against the secure dashboard.
func requestScheme(r *http.Request, trustedProxy bool) string {
	if r.TLS != nil {
		return "https"
	}
	if trustedProxy && r.Header.Get("X-Forwarded-Proto") == "https" {
		return "https"
	}
	return "http"
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
	// [R247-SEC-1] Compute the request scheme once; both Origin and
	// Referer paths must match it so an HTTPS request cannot be paired
	// with an `Origin: http://host` (mixed-content downgrade) and vice
	// versa. Without this an attacker on a sibling http:// host can
	// pass the host-only gate against an https:// dashboard.
	reqScheme := requestScheme(r, trustedProxy)
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
		// [R247-SEC-1] Referer scheme must match the request scheme.
		if u.Scheme != reqScheme {
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
	// [R247-SEC-1] Origin scheme must match the request scheme so a
	// downgrade attack from a sibling http:// origin cannot satisfy the
	// gate against an https:// dashboard.
	if u.Scheme != reqScheme {
		return false
	}
	return u.Host == host
}
