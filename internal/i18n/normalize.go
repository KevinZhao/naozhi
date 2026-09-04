package i18n

import "strings"

// NormalizeLocale canonicalizes a locale string to "zh-CN" / "en-US" for
// whitelisted inputs and "" otherwise: trim + lowercase, strip POSIX encoding
// (".utf-8") and modifier ("@euro") suffixes, "_" → "-", then match the
// "zh"/"zh-*" and "en"/"en-*" prefixes.
func NormalizeLocale(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}

	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		s = s[:dot]
	}

	if at := strings.IndexByte(s, '@'); at >= 0 {
		s = s[:at]
	}

	s = strings.ReplaceAll(s, "_", "-")

	switch {
	case s == "zh" || strings.HasPrefix(s, "zh-"):
		return "zh-CN"
	case s == "en" || strings.HasPrefix(s, "en-"):
		return "en-US"
	default:
		return ""
	}
}
