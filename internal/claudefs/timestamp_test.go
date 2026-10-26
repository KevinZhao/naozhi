package claudefs

import "testing"

// TestTimestampMillis_Accepted pins unix-ms values worked out independently of
// time.Parse, for the shapes the Claude CLI writes and for zone offsets. The
// epoch is 0 like the sentinel, so ParseTimestamp's ok is checked too.
func TestTimestampMillis_Accepted(t *testing.T) {
	cases := map[string]int64{
		"2026-05-26T07:16:17Z":            1779779777000,
		"2026-05-26T07:16:17.123Z":        1779779777123,
		"2026-05-26T07:16:17.1234567890Z": 1779779777123, // time.Parse takes >9 digits
		"2026-05-26T15:16:17+08:00":       1779779777000,
		"1970-01-01T00:00:00Z":            0,
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
// a line: malformed shapes, and calendar fields out of range, which must be
// rejected rather than normalised (month 13 is not next January).
func TestTimestampMillis_RejectedIsZero(t *testing.T) {
	rejects := []string{
		"",
		"not-a-time",
		"2026-05-26T07:16:17",  // no zone
		"2026-13-26T07:16:17Z", // month 13
		"2026-02-29T07:16:17Z", // Feb 29 in a non-leap year
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
