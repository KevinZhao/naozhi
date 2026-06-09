package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerifyChecksum_CommentLineSkipped pins R20260608133928-SEC-6:
// a line of the form "# asset" must not be accepted as a checksum entry.
// Previously fields[0]="#" would be stored as expected and produce a
// misleading "checksum mismatch: expected #" rather than a clean rejection.
func TestVerifyChecksum_CommentLineSkipped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	content := []byte("binary data")
	binPath := filepath.Join(dir, "naozhi-linux-amd64")
	if err := os.WriteFile(binPath, content, 0o644); err != nil {
		t.Fatal(err)
	}

	// Comment-style line: fields[0]="#", fields[1]="naozhi-linux-amd64"
	sums := "# naozhi-linux-amd64\n"
	sumPath := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(sumPath, []byte(sums), 0o644); err != nil {
		t.Fatal(err)
	}

	err := verifyChecksum(binPath, sumPath, "naozhi-linux-amd64")
	if err == nil {
		t.Fatal("expected error (no valid checksum entry), got nil")
	}
	// The error must NOT say "expected #" — that would indicate the comment
	// was accepted as a hash value.
	if strings.Contains(err.Error(), "expected #") {
		t.Errorf("comment line was incorrectly accepted as checksum: %v", err)
	}
}

// TestVerifyChecksum_NonHexFieldRejected pins R20260608133928-SEC-6:
// a checksums.txt field that is not valid hex (or not 64 chars) must be
// rejected with an explicit error rather than stored as expected.
func TestVerifyChecksum_NonHexFieldRejected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		field0 string
	}{
		{"too short", strings.Repeat("a", 32)},
		{"too long", strings.Repeat("a", 65)},
		{"non-hex chars", strings.Repeat("z", 64)},
		{"mixed junk", "!!" + strings.Repeat("a", 62)},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()

			content := []byte("binary")
			binPath := filepath.Join(dir, "naozhi-linux-amd64")
			if err := os.WriteFile(binPath, content, 0o644); err != nil {
				t.Fatal(err)
			}

			sums := fmt.Sprintf("%s  naozhi-linux-amd64\n", tc.field0)
			sumPath := filepath.Join(dir, "checksums.txt")
			if err := os.WriteFile(sumPath, []byte(sums), 0o644); err != nil {
				t.Fatal(err)
			}

			err := verifyChecksum(binPath, sumPath, "naozhi-linux-amd64")
			if err == nil {
				t.Fatalf("expected error for malformed field %q, got nil", tc.field0)
			}
		})
	}
}

// TestVerifyChecksum_ValidHexAccepted confirms that a well-formed 64-char
// hex checksum still passes the new guard and the full integrity check.
func TestVerifyChecksum_ValidHexAccepted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	content := []byte("legitimate binary content")
	binPath := filepath.Join(dir, "naozhi-linux-amd64")
	if err := os.WriteFile(binPath, content, 0o644); err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(content)
	hexSum := hex.EncodeToString(sum[:])
	sums := fmt.Sprintf("%s  naozhi-linux-amd64\n", hexSum)
	sumPath := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(sumPath, []byte(sums), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := verifyChecksum(binPath, sumPath, "naozhi-linux-amd64"); err != nil {
		t.Errorf("valid checksum unexpectedly rejected: %v", err)
	}
}
