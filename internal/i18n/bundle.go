// Package i18n provides locale resolution and message rendering for naozhi.
// It is zero-HTTP: callers parse requests into plain strings before calling
// ResolveDashboard.
package i18n

import (
	"sync"

	"golang.org/x/text/language"
)

// HeuristicCfg controls the CJK rune-ratio language guess.
type HeuristicCfg struct {
	Enabled      bool
	CJKThreshold float64
	MinRunes     int
}

// Bundle holds all locales. Immutable after construction; concurrent T() is
// safe. Printer holds only *Bundle (not map refs) so a future Reload can swap
// an atomic.Pointer[Bundle] without invalidating live Printers.
type Bundle struct {
	defaultLocale string
	supported     []string
	heuristicCfg  HeuristicCfg
	msgs          map[string]map[string]*compiledTemplate

	// matcherOnce/cachedMatcher memoize the language.Matcher: the Bundle is
	// immutable and Matcher is concurrency-safe, so build it once per Bundle
	// rather than per request.
	matcherOnce   sync.Once
	cachedMatcher language.Matcher
}

// NewForTest builds a Bundle from an in-memory map (locale → key → template),
// bypassing YAML/embed. Default locale prefers "zh-CN" (see pickDefault);
// heuristic config uses {true, 0.3, 4}.
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

// pickDefault prefers "zh-CN", then "en-US", else the first locale, else "zh-CN".
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

// For returns a locale-bound Printer. An unknown locale is not rejected; its
// T falls back to "[key]" for every key.
func (b *Bundle) For(locale string) *Printer {
	return &Printer{locale: locale, bundle: b}
}
