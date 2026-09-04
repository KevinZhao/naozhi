package auth

import (
	"net/http"
	"net/url"
	"strings"
)

// IsSafeMethod reports whether the HTTP method is safe per RFC 7231 §4.2.1.
// The CSRF Origin gate applies only to mutating methods so GET prefetches,
// HEAD probes and CORS preflight OPTIONS keep working.
func IsSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// RequestHost returns the effective host the browser (or trusted proxy)
// addressed. With TrustedProxy and X-Forwarded-Host present it takes the LAST
// comma-separated value — appended by the trusted proxy, unspoofable by the
// client — mirroring netutil.ClientIP's last-XFF semantics.
func RequestHost(r *http.Request, trustedProxy bool) string {
	host := r.Host
	if trustedProxy {
		if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
			tail := fwd
			if i := strings.LastIndexByte(fwd, ','); i >= 0 {
				tail = fwd[i+1:]
			}
			tail = strings.TrimSpace(tail)
			if tail != "" {
				host = tail
			}
		}
	}
	return host
}

// SameOriginOK reports whether the Origin (or Referer fallback) identifies the
// same host naozhi serves on. Missing Origin AND Referer passes: non-browser
// clients carry no victim cookies. Callers must restrict this to mutating
// methods (IsSafeMethod). Defense-in-depth against same-registrable-domain
// attackers that SameSite=Strict does not stop.
//
// Scheme is intentionally NOT compared: the cookie has no Domain attribute and
// is SameSite=Strict, so a scheme gate adds nothing while breaking HTTP-only
// CDN→origin hops where X-Forwarded-Proto arrives as http.
func SameOriginOK(r *http.Request, trustedProxy bool) bool {
	host := RequestHost(r, trustedProxy)
	if host == "" {
		// Unknown Host: nothing to validate against, fail closed.
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Referer fallback: some browsers omit Origin on legacy same-origin POSTs.
		ref := r.Header.Get("Referer")
		if ref == "" {
			return true
		}
		u, err := url.Parse(ref)
		if err != nil || u.Host == "" {
			return false
		}
		// Non-http(s) Referer schemes (javascript:, data:, ftp:, file:, blob:)
		// can parse with the correct host but must not count as same-origin.
		if u.Scheme != "http" && u.Scheme != "https" {
			return false
		}
		return u.Host == host
	}
	// RFC 6454 "null" (opaque origins: sandboxed iframes, file://) is a
	// definite cross-origin.
	if origin == "null" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	// Same scheme guard as the Referer fallback above.
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host == host
}
