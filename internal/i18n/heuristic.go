package i18n

import (
	"unicode"
	"unicode/utf8"
)

// Heuristic infers "zh-CN" vs "en-US" from the CJK rune ratio of text.
// Returns ("", false) when disabled or text has fewer than cfg.MinRunes runes;
// otherwise ("zh-CN", true) when the ratio meets cfg.CJKThreshold, else
// ("en-US", true).
func (b *Bundle) Heuristic(text string) (locale string, confident bool) {
	cfg := b.heuristicCfg
	if !cfg.Enabled {
		return "", false
	}

	total := utf8.RuneCountInString(text)
	if total < cfg.MinRunes {
		return "", false
	}

	cjk := 0
	for _, r := range text {
		if isCJK(r) {
			cjk++
		}
	}

	ratio := float64(cjk) / float64(total)
	if ratio >= cfg.CJKThreshold {
		return "zh-CN", true
	}
	return "en-US", true
}

// isCJK reports whether r is Han, Hiragana, Katakana or Hangul.
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}
