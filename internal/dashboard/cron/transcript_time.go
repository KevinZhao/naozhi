package cron

import "time"

// parseISO8601MS converts an RFC 3339 / ISO 8601 timestamp into unix ms.
// Returns 0 when the input is empty or unparseable so callers can use it as a
// "skip filter" sentinel. time.RFC3339Nano is a strict superset of RFC3339
// (the fractional part is optional), so no second layout is needed.
//
// The Claude CLI exclusively emits "YYYY-MM-DDTHH:MM:SS[.fff…]Z", which the
// byte-level fast path parses in ~30ns vs ~300ns for time.Parse — compounding
// across 500-line transcripts under bulk polling (#1012). Anything
// non-canonical (offsets, exotic layouts) falls back to time.Parse, so
// results are bit-identical to the slow path.
func parseISO8601MS(s string) int64 {
	if s == "" {
		return 0
	}
	if ms, ok := parseISO8601MSFast(s); ok {
		return ms
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// parseISO8601MSFast hand-parses the canonical UTC RFC 3339 shape the Claude
// CLI emits and returns (unixMillis, true) on success:
//
//	YYYY-MM-DDTHH:MM:SS(.fffffffff)?Z
//
// Any deviation (offset other than 'Z', missing field, non-digit, lowercase
// 't'/'z') returns (0, false) and the caller falls back to time.Parse. Each
// field is range-checked before time.Date because time.Date *normalises*
// out-of-range values (month 13 → next January) whereas time.Parse rejects
// them; seconds cap at 59 since time.Parse does not honour leap seconds.
func parseISO8601MSFast(s string) (int64, bool) {
	// Minimum canonical length is "YYYY-MM-DDTHH:MM:SSZ" = 20 bytes.
	if len(s) < 20 {
		return 0, false
	}
	// Fixed-position separator check before any digit work.
	if s[4] != '-' || s[7] != '-' || s[10] != 'T' ||
		s[13] != ':' || s[16] != ':' {
		return 0, false
	}
	year, ok := parseDigits(s[0:4])
	if !ok {
		return 0, false
	}
	month, ok := parseDigits(s[5:7])
	if !ok {
		return 0, false
	}
	day, ok := parseDigits(s[8:10])
	if !ok {
		return 0, false
	}
	hour, ok := parseDigits(s[11:13])
	if !ok {
		return 0, false
	}
	minute, ok := parseDigits(s[14:16])
	if !ok {
		return 0, false
	}
	second, ok := parseDigits(s[17:19])
	if !ok {
		return 0, false
	}
	// Range-check so we reject what time.Parse rejects instead of letting
	// time.Date normalise; day is validated per-month (leap-year aware).
	if month < 1 || month > 12 ||
		day < 1 || day > daysInMonth(year, month) ||
		hour > 23 ||
		minute > 59 ||
		second > 59 {
		return 0, false
	}
	// After SS we expect either:
	//   - "Z"          (no fractional seconds)
	//   - ".<digits>Z" (1..9 fractional digits, RFC3339Nano)
	nanos := 0
	rest := s[19:]
	if rest[0] == '.' {
		// Find the trailing 'Z' and require 1..9 fractional digits.
		if len(rest) < 3 { // need at least ".dZ"
			return 0, false
		}
		// Locate Z and verify all interior chars are digits.
		fracEnd := -1
		for i := 1; i < len(rest); i++ {
			c := rest[i]
			if c == 'Z' {
				fracEnd = i
				break
			}
			if c < '0' || c > '9' {
				return 0, false
			}
		}
		if fracEnd < 2 || fracEnd != len(rest)-1 {
			return 0, false
		}
		fracDigits := rest[1:fracEnd]
		if len(fracDigits) > 9 {
			return 0, false
		}
		// Convert fractional seconds into nanoseconds. Pad on the right
		// with implicit zeros so ".5" → 500000000ns, ".123" → 123000000ns.
		nanos, ok = parseDigits(fracDigits)
		if !ok {
			return 0, false
		}
		for i := len(fracDigits); i < 9; i++ {
			nanos *= 10
		}
	} else if rest == "Z" {
		// canonical SS Z, no fractional seconds.
	} else {
		return 0, false
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, nanos, time.UTC)
	return t.UnixMilli(), true
}

// daysInMonth returns the number of days in the given (year, month). month
// is assumed to already be in 1..12. February is leap-year aware so the
// fast path's day range-check matches time.Parse's calendar validation.
func daysInMonth(year, month int) int {
	switch month {
	case 1, 3, 5, 7, 8, 10, 12:
		return 31
	case 4, 6, 9, 11:
		return 30
	default: // February
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			return 29
		}
		return 28
	}
}

// parseDigits parses a fixed-length all-ASCII-digits string as a non-negative
// int. Returns (n, true) on success, (0, false) on any non-digit. Beats
// strconv.Atoi by skipping the leading-sign / leading-zero handling.
func parseDigits(s string) (int, bool) {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
