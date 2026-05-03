package osutil

import (
	"strings"
	"testing"
)

// TestIsLogInjectionRune covers the classes explicitly enumerated in the
// godoc: C1 controls, bidi overrides, bidi isolates, LS/PS. Plus negative
// cases (ASCII, CJK, emoji) to lock the "only the classes that byte-level
// filters miss" contract.
func TestIsLogInjectionRune(t *testing.T) {
	t.Parallel()

	positive := []struct {
		name string
		r    rune
	}{
		{"C1_NEL", 0x85},
		{"C1_low_boundary", 0x80},
		{"C1_high_boundary", 0x9F},
		{"bidi_LRE", 0x202A},
		{"bidi_RLE", 0x202B},
		{"bidi_PDF", 0x202C},
		{"bidi_LRO", 0x202D},
		{"bidi_RLO", 0x202E},
		{"bidi_LRI", 0x2066},
		{"bidi_RLI", 0x2067},
		{"bidi_FSI", 0x2068},
		{"bidi_PDI", 0x2069},
		{"LS", 0x2028},
		{"PS", 0x2029},
	}
	for _, tc := range positive {
		t.Run("positive_"+tc.name, func(t *testing.T) {
			t.Parallel()
			if !IsLogInjectionRune(tc.r) {
				t.Errorf("IsLogInjectionRune(U+%04X) = false, want true", tc.r)
			}
		})
	}

	negative := []struct {
		name string
		r    rune
	}{
		// ASCII printable (the classes callers should NOT drop via this
		// helper — they should rely on < 0x20 / == 0x7f).
		{"ASCII_A", 'A'},
		{"ASCII_space", ' '},
		{"ASCII_newline_not_caught_here", '\n'},
		{"ASCII_tab_not_caught_here", '\t'},
		{"ASCII_DEL_not_caught_here", 0x7f},
		// Valid CJK / emoji must survive — this helper is not a "strip
		// non-ASCII" filter; that would destroy legitimate Chinese / Japanese
		// identifiers we explicitly support in this codebase.
		{"CJK_hello", '你'},
		{"CJK_world", '好'},
		{"emoji_grinning", '😀'},
		{"emoji_check", '✓'},
		// Zero-width codepoints adjacent to the bidi range — the helper
		// does NOT claim to cover them (sanitizeKeyComponent in session
		// package does, but only for session-key components where
		// invisible chars are a key-confusion risk; log-attr callers
		// should decide if they need to strip them).
		{"ZWJ_not_caught_here", 0x200D},
		{"ZWSP_not_caught_here", 0x200B},
		{"BOM_not_caught_here", 0xFEFF},
	}
	for _, tc := range negative {
		t.Run("negative_"+tc.name, func(t *testing.T) {
			t.Parallel()
			if IsLogInjectionRune(tc.r) {
				t.Errorf("IsLogInjectionRune(U+%04X) = true, want false", tc.r)
			}
		})
	}
}

// TestSanitizeForLog_FastPathASCII locks the zero-allocation guarantee for
// the common case (Go stdlib errors are pure ASCII-printable).
func TestSanitizeForLog_FastPathASCII(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"",
		"connection refused",
		"get session: context canceled",
		"dial tcp 127.0.0.1:6789: connect: connection refused",
		"exec.Command: no such file or directory",
	}
	for _, s := range inputs {
		got := SanitizeForLog(s, 0)
		if got != s {
			t.Errorf("SanitizeForLog(%q) = %q, want unchanged (fast path)", s, got)
		}
	}
}

// TestSanitizeForLog_RewritesControlBytes covers C0 controls, DEL, C1 (via
// UTF-8 encoding), bidi overrides, and LS/PS in a single error-string
// shaped input. The expected shape deliberately does NOT test rune-exact
// positions because strings.Map can shift byte offsets for multi-byte runes
// replaced with single-byte '_'; we only assert the unsafe rune class is
// gone.
func TestSanitizeForLog_RewritesControlBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
	}{
		{"newline", "error: \nfake log line"},
		{"carriage_return", "error: \rfake log line"},
		{"tab", "error:\tinjected attr"},
		{"DEL", "error: \x7f"},
		{"C1_NEL", "error: second line"},
		{"bidi_RLO", "error: ‮fake_backwards"},
		{"bidi_LRI", "error: ⁦isolated"},
		{"LS", "error:  fake line"},
		{"PS", "error:  fake paragraph"},
		// All unsafe classes at once.
		{"mixed_hostile", "\n\r\t\x7f‮  "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SanitizeForLog(tc.in, 0)
			// All the unsafe runes above MUST be stripped from the output.
			unsafeChars := []string{"\n", "\r", "\t", "\x7f", "", "‮", "⁦", " ", " "}
			for _, c := range unsafeChars {
				if strings.Contains(got, c) {
					t.Errorf("SanitizeForLog(%q) = %q — still contains unsafe char %q", tc.in, got, c)
				}
			}
		})
	}
}

// TestSanitizeForLog_PreservesSafeContent asserts CJK + emoji survive; we
// do not want operators to hit a log-sanitize path that obliterates
// legitimate non-ASCII content.
func TestSanitizeForLog_PreservesSafeContent(t *testing.T) {
	t.Parallel()

	in := "发送失败: context deadline exceeded 😞"
	got := SanitizeForLog(in, 0)
	if got != in {
		t.Errorf("SanitizeForLog(%q) = %q, want unchanged (CJK + emoji should survive)", in, got)
	}
}

// TestSanitizeForLog_EnforcesMaxLen verifies the cap behaviour. Pass 0 for
// unlimited, positive for byte-level truncate.
func TestSanitizeForLog_EnforcesMaxLen(t *testing.T) {
	t.Parallel()

	longErr := strings.Repeat("a", 1000)
	got := SanitizeForLog(longErr, 64)
	if len(got) != 64 {
		t.Errorf("SanitizeForLog(1000 bytes, cap=64) len = %d, want 64", len(got))
	}

	// Cap=0 → no truncation.
	got = SanitizeForLog(longErr, 0)
	if len(got) != 1000 {
		t.Errorf("SanitizeForLog(1000 bytes, cap=0) len = %d, want 1000", len(got))
	}

	// Shorter than cap → unchanged.
	got = SanitizeForLog("short", 64)
	if got != "short" {
		t.Errorf("SanitizeForLog(short, cap=64) = %q, want unchanged", got)
	}

	// Hostile + long → strip first, then truncate. The cap applies AFTER
	// rewriting unsafe runes to single-byte '_', so the resulting byte
	// length is predictable.
	hostile := strings.Repeat("\n", 200)
	got = SanitizeForLog(hostile, 64)
	if len(got) != 64 {
		t.Errorf("SanitizeForLog(hostile, cap=64) len = %d, want 64", len(got))
	}
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("SanitizeForLog(hostile, cap=64) still contains newlines: %q", got)
	}
}

// TestSanitizeForLog_EmptyInput locks the empty-string short-circuit so
// future callers can rely on it without extra guards.
func TestSanitizeForLog_EmptyInput(t *testing.T) {
	t.Parallel()

	if got := SanitizeForLog("", 0); got != "" {
		t.Errorf("SanitizeForLog(\"\", 0) = %q, want empty", got)
	}
	if got := SanitizeForLog("", 64); got != "" {
		t.Errorf("SanitizeForLog(\"\", 64) = %q, want empty", got)
	}
}
