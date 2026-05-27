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

// TestVerifyChecksum_DuplicateEntryRefused pins R241-SEC-15 / #474:
// the checksums.txt parser previously took the first matching line and
// silently ignored any further entries for the same asset. An attacker
// who could append to the file (e.g. via a release-asset overwrite or a
// MITM that survives the https + GitHub host pin in some pathological
// future) could thereby smuggle in a second weak/forged hash entry that
// would still parse cleanly. After this fix, a duplicate asset entry
// is a hard error — the file is treated as tampered and refused.
func TestVerifyChecksum_DuplicateEntryRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	content := []byte("fake binary content")
	binPath := filepath.Join(dir, "naozhi-linux-amd64")
	if err := os.WriteFile(binPath, content, 0o644); err != nil {
		t.Fatal(err)
	}

	realHash := sha256.Sum256(content)
	realHex := hex.EncodeToString(realHash[:])
	// Forged second hash for the same asset — a value an attacker would
	// match a planted binary against. The first line is the legitimate
	// release hash; the second line is the smuggled one.
	forgedHex := strings.Repeat("a", 64)

	sums := fmt.Sprintf("%s  naozhi-linux-amd64\n%s  naozhi-linux-amd64\n", realHex, forgedHex)
	sumPath := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(sumPath, []byte(sums), 0o644); err != nil {
		t.Fatal(err)
	}

	err := verifyChecksum(binPath, sumPath, "naozhi-linux-amd64")
	if err == nil {
		t.Fatal("expected duplicate-entry error, got nil — verifier accepted a tampered checksums.txt")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention duplicate, got: %v", err)
	}
}

// TestVerifyChecksum_DuplicateOtherAssetIgnored sanity-checks that
// duplicates for an UNRELATED asset (e.g. linux-arm64 lines while we
// are verifying linux-amd64) do not falsely trigger the refusal. The
// release format may legitimately list multiple assets, and only the
// asset under verification matters.
func TestVerifyChecksum_DuplicateOtherAssetIgnored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	content := []byte("fake amd64 binary")
	binPath := filepath.Join(dir, "naozhi-linux-amd64")
	if err := os.WriteFile(binPath, content, 0o644); err != nil {
		t.Fatal(err)
	}

	realHash := sha256.Sum256(content)
	realHex := hex.EncodeToString(realHash[:])

	// Two arm64 entries (which would themselves be malformed but
	// shouldn't affect amd64 verification — verifier scope is per-asset).
	sums := fmt.Sprintf(
		"%s  naozhi-linux-amd64\n%s  naozhi-linux-arm64\n%s  naozhi-linux-arm64\n",
		realHex, strings.Repeat("b", 64), strings.Repeat("c", 64),
	)
	sumPath := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(sumPath, []byte(sums), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := verifyChecksum(binPath, sumPath, "naozhi-linux-amd64"); err != nil {
		t.Errorf("verifyChecksum unexpectedly rejected file with unrelated-asset dups: %v", err)
	}
}
