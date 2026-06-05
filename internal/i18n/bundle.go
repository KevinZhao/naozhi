// Package i18n provides locale resolution and message rendering for naozhi.
//
// This file holds the pure-logic core (Bundle/Printer construction). It has
// zero net/http dependency: HTTP requests are parsed by callers into plain
// strings before reaching ResolveDashboard. The embed.FS + YAML Load() path,
// config wiring, and dashboard-facing helpers are intentionally deferred to a
// follow-up slice; this package currently imports only stdlib + golang.org/x/text.
package i18n

// HeuristicCfg controls the CJK rune-ratio language guess (§3.5, NNM5).
type HeuristicCfg struct {
	Enabled      bool
	CJKThreshold float64
	MinRunes     int
}

// Bundle holds all locales. Immutable after construction; concurrent T() is
// safe. There is no Reload() API in this slice. A future Reload MUST use an
// atomic.Pointer[Bundle] swap; because Printer holds only *Bundle (not map
// refs), swapping the pointer is sufficient to redirect future T() calls while
// keeping live Printer values valid (NNH1).
type Bundle struct {
	defaultLocale string
	supported     []string
	heuristicCfg  HeuristicCfg
	msgs          map[string]map[string]*compiledTemplate
}

// NewForTest builds a Bundle from an in-memory map (locale → key → template
// string), bypassing YAML/embed. Intended for unit tests only.
//
// The default locale and supported set are derived from the supplied messages:
// the first key found while ranging is non-deterministic, so callers that need
// a specific default should ensure "zh-CN" is present (it is treated as the
// preferred default when available). The heuristic config uses the design
// defaults ({true, 0.3, 4}).
func NewForTest(messages map[string]map[string]string) *Bundle {
	msgs := make(map[string]map[string]*compiledTemplate, len(messages))
	supported := make([]string, 0, len(messages))
	for locale, kv := range messages {
		supported = append(supported, locale)
		compiled := make(map[string]*compiledTemplate, len(kv))
		for key, tmpl := range kv {
			compiled[key] = compile(tmpl)
		}
		msgs[locale] = compiled
	}

	defaultLocale := pickDefault(supported)

	return &Bundle{
		defaultLocale: defaultLocale,
		supported:     supported,
		heuristicCfg:  HeuristicCfg{Enabled: true, CJKThreshold: 0.3, MinRunes: 4},
		msgs:          msgs,
	}
}

// pickDefault prefers "zh-CN" (the design default), then "en-US", else the
// first available locale, else "zh-CN" for an empty bundle.
func pickDefault(supported []string) string {
	for _, l := range supported {
		if l == "zh-CN" {
			return l
		}
	}
	for _, l := range supported {
		if l == "en-US" {
			return l
		}
	}
	if len(supported) > 0 {
		return supported[0]
	}
	return "zh-CN"
}

// For returns a locale-bound Printer. It does not validate locale against the
// supported set; an unknown locale yields a Printer whose T falls back to the
// "[key]" form for every key.
func (b *Bundle) For(locale string) *Printer {
	return &Printer{locale: locale, bundle: b}
}
