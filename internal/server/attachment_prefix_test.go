package server

import (
	"strings"
	"testing"
)

// TestAttachmentDirPrefixWireContract pins R241-SEC-8 (#468): the
// workspace-relative attachment prefix is a wire-format constant, NOT
// a filesystem path. Three invariants protect both ends of the
// contract — the HasPrefix gate inside handleAttachment and the
// FromSlash conversion that feeds filepath.Join after trimming.
//
// Drift on any of these would either silently widen the gate (a
// security regression) or reject legitimate EventEntry.ImagePaths on
// non-Linux hosts (a denial-of-service for the lightbox).
func TestAttachmentDirPrefixWireContract(t *testing.T) {
	t.Parallel()

	// 1. The prefix must use forward slashes only — every persisted
	//    EventEntry.ImagePaths value uses `/` regardless of OS, so
	//    the HasPrefix gate must compare against the same shape.
	if strings.ContainsRune(attachmentDirPrefix, '\\') {
		t.Fatalf("attachmentDirPrefix contains backslash: %q", attachmentDirPrefix)
	}

	// 2. The trailing slash is required. Without it, "...attachments"
	//    would also match a hypothetical workspace dir like
	//    ".naozhi/attachments-export/" and admit out-of-tree reads.
	if !strings.HasSuffix(attachmentDirPrefix, "/") {
		t.Fatalf("attachmentDirPrefix lost trailing slash: %q", attachmentDirPrefix)
	}

	// 3. The prefix is workspace-relative — must not start with `/`,
	//    otherwise the HasPrefix check would only fire on absolute
	//    wire paths and the upstream IsAbs reject filter would have
	//    already 400'd them. A leading `/` here is an obvious typo
	//    that no test would catch otherwise.
	if strings.HasPrefix(attachmentDirPrefix, "/") {
		t.Fatalf("attachmentDirPrefix is absolute, must be workspace-relative: %q", attachmentDirPrefix)
	}

	// 4. The expected literal — pin the exact value so any rename
	//    (e.g. moving attachments under a sub-dir) shows up in code
	//    review as a wire-format change, not a quiet refactor.
	const want = ".naozhi/attachments/"
	if attachmentDirPrefix != want {
		t.Fatalf("attachmentDirPrefix = %q, want %q (R241-SEC-8 wire contract)",
			attachmentDirPrefix, want)
	}
}
