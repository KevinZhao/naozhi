package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerifyPinnedChecksumsFile_Unset_R238_SEC_4 anchors #815: with the
// pin env var unset, behaviour must be identical to the pre-pin code path
// (no error, no read of checksums.txt). This is the load-bearing default
// for every existing operator who hasn't opted in.
func TestVerifyPinnedChecksumsFile_Unset_R238_SEC_4(t *testing.T) {
	// Cannot t.Parallel() because we mutate process env. Use t.Setenv so
	// the env edit is scoped to this test (cleared on completion).
	t.Setenv(pinSha256EnvVar, "")
	// Path can be bogus — the function must not even read it when unset.
	if err := verifyPinnedChecksumsFile("/nonexistent/checksums.txt"); err != nil {
		t.Fatalf("unset pin should be a no-op, got error: %v", err)
	}
}

// TestVerifyPinnedChecksumsFile_Match_R238_SEC_4 anchors the happy path:
// a correct pin matches the file's SHA-256 and the function returns nil.
func TestVerifyPinnedChecksumsFile_Match_R238_SEC_4(t *testing.T) {
	dir := t.TempDir()
	body := []byte("abcdef0123456789  naozhi-linux-amd64\n")
	sumPath := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(sumPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(body)
	t.Setenv(pinSha256EnvVar, hex.EncodeToString(h[:]))
	if err := verifyPinnedChecksumsFile(sumPath); err != nil {
		t.Fatalf("matching pin should succeed, got: %v", err)
	}
}

// TestVerifyPinnedChecksumsFile_MismatchRefused_R238_SEC_4 anchors the
// security-critical branch: a wrong pin must reject. This is what closes
// the leaked-token scenario the issue calls out — even if the attacker
// swaps both binary and checksums.txt with valid hashes that chain to
// each other, the pinned hash on checksums.txt itself does not match,
// so the upgrade aborts.
func TestVerifyPinnedChecksumsFile_MismatchRefused_R238_SEC_4(t *testing.T) {
	dir := t.TempDir()
	body := []byte("real checksums file content\n")
	sumPath := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(sumPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	// Pin to a different file's hash.
	h := sha256.Sum256([]byte("a totally different file"))
	t.Setenv(pinSha256EnvVar, hex.EncodeToString(h[:]))
	err := verifyPinnedChecksumsFile(sumPath)
	if err == nil {
		t.Fatal("mismatched pin must reject — token-swap defence requires it")
	}
	if !strings.Contains(err.Error(), "does not match pinned") {
		t.Fatalf("error should explain the pin mismatch, got: %v", err)
	}
}

// TestVerifyPinnedChecksumsFile_MalformedPinErrors_R238_SEC_4 anchors that
// a typo'd pin fails loud rather than silently downgrading to no-pin.
// "abc" is shorter than 64 hex chars and must be rejected; "z" * 64 is
// the right length but contains non-hex characters and must also reject.
func TestVerifyPinnedChecksumsFile_MalformedPinErrors_R238_SEC_4(t *testing.T) {
	dir := t.TempDir()
	sumPath := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(sumPath, []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []string{
		"abc",                   // too short
		strings.Repeat("z", 64), // right length, non-hex
		strings.Repeat("a", 63), // off-by-one
		strings.Repeat("a", 65), // off-by-one in the other direction
	}
	for _, pin := range cases {
		t.Setenv(pinSha256EnvVar, pin)
		err := verifyPinnedChecksumsFile(sumPath)
		if err == nil {
			t.Fatalf("malformed pin %q must error rather than silently downgrade to no-pin", pin)
		}
		if !strings.Contains(err.Error(), "not a 64-char hex SHA-256") {
			t.Fatalf("malformed pin %q error should explain format requirement, got: %v", pin, err)
		}
	}
}

// TestVerifyPinnedChecksumsFile_UppercasePinAccepted anchors that operators
// who copy/paste a pin from a tool that emitted uppercase hex (sha256sum
// on some BSDs, vendor security-bulletin tables) still match. The
// regex matches case-insensitively and the comparison ToLowers both sides.
func TestVerifyPinnedChecksumsFile_UppercasePinAccepted(t *testing.T) {
	dir := t.TempDir()
	body := []byte("checksums body\n")
	sumPath := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(sumPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(body)
	upper := strings.ToUpper(hex.EncodeToString(h[:]))
	t.Setenv(pinSha256EnvVar, upper)
	if err := verifyPinnedChecksumsFile(sumPath); err != nil {
		t.Fatalf("uppercase pin should match (case-insensitive), got: %v", err)
	}
}
