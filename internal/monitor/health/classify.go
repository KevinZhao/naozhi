package health

import (
	"regexp"
	"strings"

	"github.com/naozhi/naozhi/internal/apierr"
)

// ErrorClass is a failure's category, the key systemic-fault merging groups
// by (docs/rfc/planner.md §15.1). Values are wire-stable: they are written to
// the journal.
type ErrorClass string

const (
	ErrNone ErrorClass = ""
	// ErrAuth: credentials rejected or permission denied (403, invalid key).
	ErrAuth ErrorClass = "auth"
	// ErrRegion: the request was refused for the caller's country or region.
	ErrRegion ErrorClass = "region"
	// ErrRateLimit: throttled (429).
	ErrRateLimit ErrorClass = "rate_limit"
	// ErrNetwork: DNS, connection or transport failures.
	ErrNetwork ErrorClass = "network"
	// ErrCredentialExpired: a Midway / SSO session or temporary token expired.
	ErrCredentialExpired ErrorClass = "credential_expired"
	// ErrDiskFull: no space left on a device.
	ErrDiskFull ErrorClass = "disk_full"
	// ErrRefusal: the model declined to answer.
	ErrRefusal ErrorClass = "refusal"
	// ErrOther: a failure none of the above names.
	ErrOther ErrorClass = "other"
)

// patterns are matched, in order, against the lowercased text before
// apierr's envelope categories: the more specific causes come first because
// an expired token or a region refusal often also says 403 / 400.
var patterns = []struct {
	class ErrorClass
	subs  []string
}{
	{ErrDiskFull, []string{"no space left on device", "enospc", "disk full", "disk quota exceeded"}},
	{ErrCredentialExpired, []string{"expiredtoken", "token has expired", "token is expired", "security token included in the request is expired",
		"sso session", "midway", "mwinit", "credentials have expired", "refresh token"}},
	{ErrRegion, []string{"unsupported countr", "unsupported_country", "not available in your region", "region is not supported"}},
	{ErrRefusal, []string{"model_refusal", "refusal"}},
}

// fallbacks are matched after apierr's categories found nothing: transport
// phrases, then throttling and denial words, which are too common in
// ordinary text to beat a recognised envelope. Network goes first so a
// refused connection is not read as a denial.
var fallbacks = []struct {
	class ErrorClass
	subs  []string
}{
	{ErrNetwork, []string{"no such host", "dial tcp", "connection refused", "connection reset", "enotfound", "getaddrinfo",
		"network is unreachable", "i/o timeout", "tls handshake"}},
	{ErrRateLimit, []string{"too many requests", "throttlingexception", "throttled"}},
	{ErrAuth, []string{"accessdenied", "access denied", "forbidden", "unauthorized", "not authorized"}},
}

// statusRe finds a bare HTTP status the fallbacks' words did not accompany;
// word boundaries keep a port or byte count (":4013") from matching.
var statusRe = regexp.MustCompile(`\b(401|403|429)\b`)

// Classify returns the category of a failure's text; ErrOther when nothing
// matches, including empty text. Only the category leaves this function: the
// text may carry prompts, paths or keys.
func Classify(text string) ErrorClass {
	lower := strings.ToLower(text)
	if c := match(lower, patterns); c != ErrNone {
		return c
	}
	if k, ok := apierr.ClassifyError(text); ok {
		switch k {
		case apierr.KindRateLimit:
			return ErrRateLimit
		case apierr.KindAuth, apierr.KindPermission:
			return ErrAuth
		case apierr.KindNetwork, apierr.KindTimeout:
			return ErrNetwork
		}
	}
	if c := match(lower, fallbacks); c != ErrNone {
		return c
	}
	switch statusRe.FindString(lower) {
	case "429":
		return ErrRateLimit
	case "401", "403":
		return ErrAuth
	}
	return ErrOther
}

func match(lower string, table []struct {
	class ErrorClass
	subs  []string
}) ErrorClass {
	for _, p := range table {
		for _, s := range p.subs {
			if strings.Contains(lower, s) {
				return p.class
			}
		}
	}
	return ErrNone
}
