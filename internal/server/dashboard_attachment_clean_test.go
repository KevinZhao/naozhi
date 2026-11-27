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
