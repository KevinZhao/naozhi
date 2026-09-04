package i18n

import "golang.org/x/text/language"

// Locale source tier constants.
const (
	sourceUser      = "user"
	sourcePlatform  = "platform"
	sourceHeuristic = "heuristic"
)

// IMResolveInput collects the inputs for ResolveIM.
type IMResolveInput struct {
	PlatformHint string // transport-provided, e.g. Slack users.info.locale
	PrevLocale   string // session.Locale
	PrevSource   string // session.LocaleSource ("user"|"platform"|"heuristic"|"")
	MessageText  string // for the CJK heuristic
	// UserOverride is absent: /lang commands short-circuit in the dispatcher.
}

// ResolveIM walks user-lock > platform hint > carried platform > heuristic >
// carried heuristic > default; the returned locale is always supported.
func (b *Bundle) ResolveIM(in IMResolveInput) (locale, source string) {
	// User lock: never overridden by any automatic source.
	if in.PrevSource == sourceUser && in.PrevLocale != "" {
		return in.PrevLocale, sourceUser
	}

	// A fresh platform hint wins even if equal to prev.
	if in.PlatformHint != "" {
		if normalized := NormalizeLocale(in.PlatformHint); normalized != "" {
			return normalized, sourcePlatform
		}
	}

	if in.PrevSource == sourcePlatform && in.PrevLocale != "" {
		return in.PrevLocale, sourcePlatform
	}

	if h, confident := b.Heuristic(in.MessageText); confident {
		return h, sourceHeuristic
	}

	if in.PrevSource == sourceHeuristic && in.PrevLocale != "" {
		return in.PrevLocale, sourceHeuristic
	}

	// Default tier carries an empty source.
	return b.defaultLocale, ""
}

// ResolveDashboard walks query > cookie > Accept-Language > default. Query and
// cookie go through the NormalizeLocale whitelist; Accept-Language is parsed
// q-value aware by x/text and matched against the supported set.
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

// matchAcceptLanguage returns the best supported locale for an Accept-Language
// header, or "" when nothing maps onto the whitelist.
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
	// language.No is a miss: fall through to the default, not index 0.
	if conf == language.No {
		return ""
	}
	return NormalizeLocale(b.supportedTags()[idx].String())
}

// supportedTags returns the supported locales as language.Tags, default first
// so the Matcher prefers it as fallback; malformed entries are skipped.
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

// languageMatcher lazily builds the Matcher once; the Bundle is immutable so it
// stays valid for the Bundle's lifetime.
func (b *Bundle) languageMatcher() language.Matcher {
	b.matcherOnce.Do(func() {
		b.cachedMatcher = language.NewMatcher(b.supportedTags())
	})
	return b.cachedMatcher
}
