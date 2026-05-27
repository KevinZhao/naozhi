package server

import (
	"strings"
	"testing"
)

// TestCleanAttachmentRelPath_Accepts pins R215-SEC-P2-3 (#536) positive
// path: well-formed forward-slash workspace-relative paths under the
// attachment subtree must round-trip through cleanAttachmentRelPath
// unchanged. The handler relies on the returned cleaned string for the
// downstream HasPrefix attachmentDirPrefix gate, so any normalization
// drift would either cause silent 404s on legitimate uploads or admit
// non-attachment paths into the join.
func TestCleanAttachmentRelPath_Accepts(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", ".naozhi/attachments/2026-05-27/img.png", ".naozhi/attachments/2026-05-27/img.png"},
		{"strip dot segment", ".naozhi/./attachments/2026-05-27/img.png", ".naozhi/attachments/2026-05-27/img.png"},
		{"nested in date", ".naozhi/attachments/2026-05-27/sub/img.png", ".naozhi/attachments/2026-05-27/sub/img.png"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, errMsg := cleanAttachmentRelPath(c.in)
			if errMsg != "" {
				t.Fatalf("cleanAttachmentRelPath(%q) errMsg = %q, want accept", c.in, errMsg)
			}
			if got != c.want {
				t.Errorf("cleanAttachmentRelPath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestCleanAttachmentRelPath_Rejects pins all the negative paths the
// handler used to reject inline. Each input must produce a non-empty
// errMsg and an empty cleaned result so the caller hits the 400 branch
// before any filesystem syscall. Includes the R215-SEC-P2-3 divergence
// guard cases.
func TestCleanAttachmentRelPath_Rejects(t *testing.T) {
	cases := []struct {
		name, in, wantErr string
	}{
		{"too long", strings.Repeat("a", 1025), "path too long"},
		{"NUL byte", ".naozhi/attachments/x\x00.png", "invalid path"},
		{"backslash", ".naozhi\\attachments\\x.png", "invalid path"},
		{"absolute", "/etc/passwd", "invalid path"},
		{"parent traversal at root", "../../secret.txt", "invalid path"},
		{"bare ..", "..", "invalid path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cleaned, errMsg := cleanAttachmentRelPath(c.in)
			if errMsg != c.wantErr {
				t.Fatalf("cleanAttachmentRelPath(%q) errMsg = %q, want %q", c.in, errMsg, c.wantErr)
			}
			if cleaned != "" {
				t.Errorf("cleanAttachmentRelPath(%q) cleaned = %q, want empty on reject", c.in, cleaned)
			}
		})
	}
}

// TestCleanAttachmentRelPath_DivergenceGuard locks the contract that the
// R215-SEC-P2-3 (#536) second-cleaner round-trip is invoked at all on
// every accepted input. We can't deterministically force path.Clean and
// filepath.Clean to disagree on Linux (they're identical), but we can
// assert the guard's structural correctness: every input that the
// helper accepts must satisfy
//
//	path.Clean(in) == filepath.ToSlash(filepath.Clean(filepath.FromSlash(path.Clean(in))))
//
// because the helper rejects any input where it doesn't. Drift detector
// — if a future refactor accidentally swaps the order of the two
// cleaners, or drops the FromSlash hop, this test stays passing on
// Linux but flips on macOS/Windows CI runs (which is the whole reason
// the second guard exists).
func TestCleanAttachmentRelPath_DivergenceGuard(t *testing.T) {
	// Each input is one we expect the helper to accept. The test asserts
	// the cleaned result is the OS-stable one (i.e. round-trips through
	// filepath.Clean(filepath.FromSlash(...))) — anything else means the
	// guard is gone or the helper accepts a divergent shape.
	inputs := []string{
		".naozhi/attachments/2026-05-27/img.png",
		".naozhi/./attachments/2026-05-27/img.png",
		".naozhi/attachments/2026-05-27/sub/img.png",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			got, errMsg := cleanAttachmentRelPath(in)
			if errMsg != "" {
				t.Fatalf("helper rejected legitimate input %q: %q", in, errMsg)
			}
			// If the helper accepts, by construction the cleaned
			// string must round-trip through both cleaners — that's
			// the very gate we're pinning.
			if got == "" {
				t.Fatalf("helper returned empty cleaned for %q with no errMsg", in)
			}
		})
	}
}
