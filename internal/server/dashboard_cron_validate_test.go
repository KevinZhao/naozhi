package server

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/osutil"
)

// TestValidateCronWorkDir_RejectsASCIIControl pins the original byte-level
// gate: NUL / CR / LF / DEL and every < 0x20 byte must be rejected so a log
// pipeline using newline framing cannot be corrupted by an authenticated
// operator pasting a CR/LF into work_dir.
func TestValidateCronWorkDir_RejectsASCIIControl(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"NUL":     "/tmp/\x00bad",
		"CR":      "/tmp/\rbad",
		"LF":      "/tmp/\nbad",
		"ESC":     "/tmp/\x1bbad",
		"DEL":     "/tmp/\x7fbad",
		"C0_max":  "/tmp/\x1fbad",
		"zero_ok": "/tmp/ok",
	}
	for name, wd := range cases {
		name, wd := name, wd
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := validateCronWorkDir(wd)
			if name == "zero_ok" {
				if err != nil {
					t.Errorf("expected nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Errorf("expected rejection for %q, got nil", wd)
			}
		})
	}
}

// TestValidateCronWorkDir_RejectsUnicodeBidi pins the Round 170 rune-level
// pass: bidi override / embedding / directional isolate codepoints + Unicode
// line separators must be rejected because they encode as valid UTF-8 with
// all bytes >= 0x20 and therefore slip past the ASCII loop. Without this
// check a path like "/tmp/safe‮/../etc" would pass validation and land
// in slog output, where terminal viewers would render it backwards.
func TestValidateCronWorkDir_RejectsUnicodeBidi(t *testing.T) {
	t.Parallel()
	bad := []struct {
		name string
		s    string
	}{
		{"RLO U+202E", "/tmp/safe‮/etc"},
		{"LRO U+202D", "/tmp/‭maybe"},
		{"LRE U+202A", "/tmp/‪maybe"},
		{"PDF U+202C", "/tmp/‬maybe"},
		{"LRI U+2066", "/tmp/⁦maybe"},
		{"RLI U+2067", "/tmp/⁧maybe"},
		{"FSI U+2068", "/tmp/⁨maybe"},
		{"PDI U+2069", "/tmp/⁩maybe"},
		{"LS U+2028", "/tmp/ok bad"},
		{"PS U+2029", "/tmp/ok bad"},
		{"C1_0x80", "/tmp/bad"},
		{"C1_0x9F", "/tmp/bad"},
	}
	for _, tc := range bad {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := validateCronWorkDir(tc.s); err == nil {
				t.Errorf("expected rejection for %q, got nil", tc.s)
			}
		})
	}
}

// TestValidateCronWorkDir_LengthCap locks the 1 KiB byte cap so a multi-MB
// workdir cannot be echoed into slog attrs on the validateWorkspace error
// path (log-flood from an authenticated attacker).
func TestValidateCronWorkDir_LengthCap(t *testing.T) {
	t.Parallel()
	long := "/tmp/" + strings.Repeat("a", maxCronWorkDirBytesDashboard)
	if err := validateCronWorkDir(long); err == nil {
		t.Error("expected rejection for >1024-byte workdir, got nil")
	}
	short := "/tmp/" + strings.Repeat("a", 100)
	if err := validateCronWorkDir(short); err != nil {
		t.Errorf("expected accept for 100+5-byte workdir, got %v", err)
	}
}

// TestValidateCronPrompt_RejectsUnicodeBidi mirrors the workdir test for the
// prompt path. Bidi overrides corrupt log output (journalctl, activity feed)
// and could confuse reviewers about what the prompt actually says before the
// ticker fires.
func TestValidateCronPrompt_RejectsUnicodeBidi(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		s    string
	}{
		{"RLO", "do the task‮ please"},
		{"LRI", "do the⁦task"},
		{"C1", "do  tricks"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := validateCronPrompt(tc.s); err == nil {
				t.Errorf("expected rejection for %q, got nil", tc.s)
			}
		})
	}
	// LF is allowed — multi-line playbook prompts are the reason this
	// validator was relaxed. Cron prompts flow to the CLI via stdin as a
	// stream-json user message, where json.Marshal escapes embedded \n.
	if err := validateCronPrompt("step 1:\n\tchild"); err != nil {
		t.Errorf("LF should be allowed in multi-line prompts, got %v", err)
	}
	// CR is still rejected — `tail -f` / `journalctl` treat it as carriage
	// return and overwrite the current log line, a log-poisoning surface
	// unrelated to stream-json framing.
	if err := validateCronPrompt("step 1\rclobber"); err == nil {
		t.Error("CR should still be rejected (log overwrite hazard)")
	}
	// NUL is still rejected — execve silently truncates at the first NUL.
	if err := validateCronPrompt("step 1\x00child"); err == nil {
		t.Error("NUL should still be rejected (execve truncation)")
	}
	// Tab alone is allowed for indentation.
	if err := validateCronPrompt("step 1\tchild"); err != nil {
		t.Errorf("tab should be allowed in prompts, got %v", err)
	}
}

// TestValidateCronWorkDir_RejectsRelativePath locks the R172-SEC-L1
// defense-in-depth check: even if validateWorkspace later loosens its
// IsAbs enforcement, the cron handler itself rejects `.`, `./foo`,
// `foo/bar`, or bare `foo`. Catches the class of bug where an upstream
// validator change silently admits workspace-relative paths.
func TestValidateCronWorkDir_RejectsRelativePath(t *testing.T) {
	t.Parallel()
	cases := []string{
		".",
		"./foo",
		"foo",
		"foo/bar",
		"../etc",
		"~/somewhere",
	}
	for _, wd := range cases {
		wd := wd
		t.Run(wd, func(t *testing.T) {
			t.Parallel()
			if err := validateCronWorkDir(wd); err == nil {
				t.Errorf("expected rejection for relative path %q, got nil", wd)
			}
		})
	}
	// Absolute paths still pass (ASCII + Unicode controls aside).
	for _, wd := range []string{"/", "/tmp", "/home/ec2-user/project"} {
		wd := wd
		t.Run("abs_"+wd, func(t *testing.T) {
			t.Parallel()
			if err := validateCronWorkDir(wd); err != nil {
				t.Errorf("expected accept for absolute path %q, got %v", wd, err)
			}
		})
	}
}

// TestIsLogInjectionRune covers the helper shared by work_dir + prompt
// validation so future callers (a third cron field, a project metadata
// field) pick up the same policy automatically.
func TestIsLogInjectionRune(t *testing.T) {
	t.Parallel()
	for _, r := range []rune{
		0x80, 0x85, 0x9F,
		0x202A, 0x202B, 0x202C, 0x202D, 0x202E,
		0x2066, 0x2067, 0x2068, 0x2069,
		0x2028, 0x2029,
	} {
		if !osutil.IsLogInjectionRune(r) {
			t.Errorf("U+%04X should be rejected", r)
		}
	}
	// Plain ASCII + non-bidi Unicode must pass.
	for _, r := range []rune{' ', 'a', '/', '中', 0x2000, 0x4E00} {
		if osutil.IsLogInjectionRune(r) {
			t.Errorf("U+%04X should be allowed", r)
		}
	}
}

// TestValidateCronFields_RejectInvalidUTF8 covers R179-GO-P1: a
// `for _, r := range s` over broken UTF-8 yields utf8.RuneError (U+FFFD)
// for each bad byte, which IsLogInjectionRune does not flag — lone
// continuation bytes could smuggle arbitrary bytes into cron_jobs.json
// and the WS broadcast. All four dashboard cron validators must
// short-circuit on utf8.ValidString==false.
func TestValidateCronFields_RejectInvalidUTF8(t *testing.T) {
	t.Parallel()
	// Lone continuation byte 0x80 is invalid UTF-8 (no leading byte);
	// 0xFE / 0xFF can never appear in valid UTF-8 at all.
	bad := []string{
		"/tmp/\x80bad",
		"/tmp/\xff\xfe",
		"\xc3", // incomplete 2-byte sequence
	}
	for _, s := range bad {
		s := s
		if err := validateCronWorkDir(s); err == nil {
			t.Errorf("validateCronWorkDir(%q) = nil, want error for invalid UTF-8", s)
		}
		if err := validateCronPrompt(s); err == nil {
			t.Errorf("validateCronPrompt(%q) = nil, want error for invalid UTF-8", s)
		}
		if err := validateCronScheduleChars(s); err == nil {
			t.Errorf("validateCronScheduleChars(%q) = nil, want error for invalid UTF-8", s)
		}
		if err := validateNotifyTarget("feishu", s); err == nil {
			t.Errorf("validateNotifyTarget(feishu,%q) = nil, want error for invalid UTF-8", s)
		}
	}
}
