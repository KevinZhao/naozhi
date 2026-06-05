package i18n

import "strings"

// NormalizeLocale canonicalizes a locale string per §3.3, returning the
// BCP-47 canonical form ("zh-CN" or "en-US") for whitelisted inputs and the
// empty string for everything else.
//
// Ordered rules:
//  1. trim spaces; lowercase
//  2. strip POSIX encoding suffix (".utf-8", ".gbk", etc.)
//  3. "_" → "-"
//  4. prefix "zh" / "zh-hans*" / "zh-hant*" / "zh-*" → "zh-CN"
//  5. prefix "en" / "en-*" → "en-US"
//  6. otherwise → ""
func NormalizeLocale(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}

	// Strip POSIX encoding suffix: everything from the first '.'.
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		s = s[:dot]
	}

	// Also strip POSIX modifier (e.g. "@euro") if present.
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
