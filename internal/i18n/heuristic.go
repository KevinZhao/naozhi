package i18n

import (
	"unicode"
	"unicode/utf8"
)

// Heuristic infers "zh-CN" vs "en-US" from the CJK rune ratio of text. It uses
// rune count (not byte length, NM4).
//
// Returns ("", false) when the heuristic is disabled OR the text has fewer than
// cfg.MinRunes runes (NNM7). Otherwise it returns ("zh-CN", true) when the CJK
// ratio meets cfg.CJKThreshold, else ("en-US", true).
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

// isCJK reports whether r belongs to a CJK ideograph range (Han) or common
// CJK symbol/Hiragana/Katakana blocks used as a proxy for East-Asian text.
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}
