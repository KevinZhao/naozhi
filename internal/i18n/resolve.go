package i18n

import "golang.org/x/text/language"

// Locale source tier constants (§3.1).
const (
	sourceUser      = "user"
	sourcePlatform  = "platform"
	sourceHeuristic = "heuristic"
)

// IMResolveInput collects all inputs for IM locale resolution. The struct form
// avoids positional-argument mistakes (NNM4).
type IMResolveInput struct {
	PlatformHint string // transport-provided, e.g. Slack users.info.locale
	PrevLocale   string // session.Locale
	PrevSource   string // session.LocaleSource ("user"|"platform"|"heuristic"|"")
	MessageText  string // for the CJK heuristic
	// UserOverride is intentionally omitted: /lang commands short-circuit in
	// the dispatcher before calling ResolveIM (see §3.1 Step 1/2).
}

// ResolveIM walks the §3.1 priority chain and always returns a valid
// (locale, source). The returned locale is guaranteed to be in the supported
// set: every branch either reuses a previously-stored value, normalizes the
// platform hint through the whitelist, derives a heuristic value, or falls back
// to the default locale.
func (b *Bundle) ResolveIM(in IMResolveInput) (locale, source string) {
	// User lock: never overridden by any automatic source.
	if in.PrevSource == sourceUser && in.PrevLocale != "" {
		return in.PrevLocale, sourceUser
	}

	// New platform value: adopt it even if equal to prev, so a user changing
	// their platform setting takes effect.
	if in.PlatformHint != "" {
		if normalized := NormalizeLocale(in.PlatformHint); normalized != "" {
			return normalized, sourcePlatform
		}
	}

	// Carry a previous platform value (no hint on this message).
	if in.PrevSource == sourcePlatform && in.PrevLocale != "" {
		return in.PrevLocale, sourcePlatform
	}

	// Heuristic on this message's text.
	if h, confident := b.Heuristic(in.MessageText); confident {
		return h, sourceHeuristic
	}

	// Carry a previous heuristic value.
	if in.PrevSource == sourceHeuristic && in.PrevLocale != "" {
		return in.PrevLocale, sourceHeuristic
	}

	// Default fallback (empty source per §3.1 bottom tier).
	return b.defaultLocale, ""
}

// ResolveDashboard walks query > cookie > Accept-Language > default. The
// signature takes plain strings so the i18n package stays zero-HTTP: callers
// extract the cookie value, the ?lang query param, and the Accept-Language
// header before calling.
//
// cookie and query are matched through NormalizeLocale (whitelist). The
// Accept-Language header is parsed with x/text (q-value aware) and matched
// against the supported set.
func (b *Bundle) ResolveDashboard(cookie, query, acceptLanguage string) string {
	if loc := NormalizeLocale(query); loc != "" {
		return loc
	}
	if loc := NormalizeLocale(cookie); loc != "" {
		return loc
	}
	if loc := b.matchAcceptLanguage(acceptLanguage); loc != "" {
		return loc
	}
	return b.defaultLocale
}

// matchAcceptLanguage parses an Accept-Language header (q-value aware via
// x/text) and returns the best supported locale, or "" when nothing maps onto
// the whitelist (or the header is unparseable).
func (b *Bundle) matchAcceptLanguage(header string) string {
	if header == "" {
		return ""
	}
	tags, _, err := language.ParseAcceptLanguage(header)
	if err != nil || len(tags) == 0 {
		return ""
	}
	matcher := b.languageMatcher()
	_, idx, conf := matcher.Match(tags...)
	// matcher always returns an index into supportedTags; idx 0 is the default.
	// Treat language.No (no usable match) as a miss so we fall through to the
	// config default in ResolveDashboard rather than silently picking index 0.
	if conf == language.No {
		return ""
	}
	return NormalizeLocale(b.supportedTags()[idx].String())
}

// supportedTags returns the supported locales as parsed language.Tags, with the
// default locale first so the x/text Matcher treats it as the preferred
// fallback. Malformed entries are skipped.
func (b *Bundle) supportedTags() []language.Tag {
	ordered := make([]string, 0, len(b.supported)+1)
	ordered = append(ordered, b.defaultLocale)
	for _, l := range b.supported {
		if l != b.defaultLocale {
			ordered = append(ordered, l)
		}
	}
	tags := make([]language.Tag, 0, len(ordered))
	for _, l := range ordered {
		if t, err := language.Parse(l); err == nil {
			tags = append(tags, t)
		}
	}
	return tags
}

func (b *Bundle) languageMatcher() language.Matcher {
	return language.NewMatcher(b.supportedTags())
}
