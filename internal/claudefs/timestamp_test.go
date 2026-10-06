package claudefs

import "testing"

// TestTimestampMillis_Accepted pins unix-ms values worked out independently of
// time.Parse, for the shapes the Claude CLI writes and for zone offsets.
// Epoch and epoch+1ns are 0 like the sentinel, so ParseTimestamp's ok is
// checked too.
func TestTimestampMillis_Accepted(t *testing.T) {
	cases := map[string]int64{
		"2026-05-26T07:16:17Z":            1779779777000,
		"2026-05-26T07:16:17.0Z":          1779779777000,
		"2026-05-26T07:16:17.5Z":          1779779777500,
		"2026-05-26T07:16:17.123Z":        1779779777123,
		"2026-05-26T07:16:17.123456Z":     1779779777123,
		"2026-05-26T07:16:17.123456789Z":  1779779777123,
		"2026-05-26T07:16:17.1234567890Z": 1779779777123, // time.Parse takes >9 digits
		"1970-01-01T00:00:00Z":            0,
		"1970-01-01T00:00:00.000000001Z":  0,
		"1969-12-31T23:59:59.999Z":        -1,
		"2099-12-31T23:59:59.999999999Z":  4102444799999,
		"2024-02-29T12:34:56Z":            1709210096000, // leap year
		"2000-02-29T00:00:00Z":            951782400000,  // leap year by the 400 rule
		"2026-01-31T23:59:59Z":            1769903999000, // last day of a 31-day month
		"2026-04-30T23:59:59Z":            1777593599000, // last day of a 30-day month
		"2026-05-26T07:16:17+00:00":       1779779777000,
		"2026-05-26T15:16:17+08:00":       1779779777000,
		"2026-05-26T07:16:17.500-05:00":   1779797777500,
	}
	for in, want := range cases {
		if got := TimestampMillis(in); got != want {
			t.Errorf("TimestampMillis(%q) = %d, want %d", in, got, want)
		}
		if _, ok := ParseTimestamp(in); !ok {
			t.Errorf("ParseTimestamp(%q) ok=false, want true", in)
		}
	}
}

// TestTimestampMillis_RejectedIsZero covers the 0 sentinel callers use to skip
// a line: malformed shapes, and calendar or clock fields out of range, which
// must be rejected rather than normalised (month 13 is not next January).
func TestTimestampMillis_RejectedIsZero(t *testing.T) {
	rejects := []string{
		"",
		"not-a-time",
		"2026-05-26T07:16:17",       // no zone
		"2026-05-26T07:16:17z",      // lowercase zone
		"2026-05-26t07:16:17Z",      // lowercase T
		"2026-05-26T07:16:17.Z",     // empty fraction
		"2026-05-26T07:16:17.12a3Z", // non-digit in fraction
		"2026/05/26T07:16:17Z",      // wrong separator
		"2026-05-26 07:16:17Z",      // space instead of T
		"26-05-26T07:16:17Z",        // 2-digit year
		"2026-5-26T07:16:17Z",       // 1-digit month
		"2024-99-01T00:00:00Z",      // month 99
		"2026-13-26T07:16:17Z",      // month 13
		"2026-00-26T07:16:17Z",      // month 0
		"2026-05-00T07:16:17Z",      // day 0
		"2026-05-32T07:16:17Z",      // day 32
		"2026-02-30T07:16:17Z",      // Feb 30
		"2026-02-29T07:16:17Z",      // Feb 29 in a non-leap year
		"2100-02-29T07:16:17Z",      // Feb 29 in a century non-leap year
		"2026-04-31T07:16:17Z",      // Apr 31
		"2026-05-26T24:00:00Z",      // hour 24
		"2026-05-26T25:00:00Z",      // hour 25
		"2026-05-26T07:60:00Z",      // minute 60
		"2026-05-26T12:00:60Z",      // leap second
		"2026-05-26T12:00:61Z",      // second 61
	}
	for _, in := range rejects {
		if got := TimestampMillis(in); got != 0 {
			t.Errorf("TimestampMillis(%q) = %d, want 0", in, got)
		}
		if _, ok := ParseTimestamp(in); ok {
			t.Errorf("ParseTimestamp(%q) ok=true, want false", in)
		}
	}
}

// FuzzTimestampMillis keeps the package's two exported timestamp entry points
// in agreement: TimestampMillis is ParseTimestamp's UnixMilli, or 0 on failure.
func FuzzTimestampMillis(f *testing.F) {
	for _, s := range []string{
		"2026-05-26T07:16:17Z", "2026-05-26T07:16:17.123456789Z",
		"2026-05-26T07:16:17.500-05:00", "2026-02-29T07:16:17Z",
		"2026-05-26T07:16:17.Z", "", "not-a-time",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		var want int64
		if ts, ok := ParseTimestamp(s); ok {
			want = ts.UnixMilli()
		}
		if got := TimestampMillis(s); got != want {
			t.Errorf("TimestampMillis(%q) = %d, ParseTimestamp gives %d", s, got, want)
		}
	})
}

func BenchmarkTimestampMillis(b *testing.B) {
	for _, in := range []string{"2026-05-26T07:16:17.123Z", "2026-05-26T07:16:17.500-05:00"} {
		b.Run(in, func(b *testing.B) {
			for range b.N {
				_ = TimestampMillis(in)
			}
		})
	}
}
