package selfupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// genKey is a tiny helper that returns a fresh ed25519 keypair, failing the
// test on the (effectively impossible) generation error.
func genKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return pub, priv
}

// signB64 signs payload with priv and base64(std)-encodes the signature, the
// on-wire form verifySignature expects.
func signB64(priv ed25519.PrivateKey, payload []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload)))
}

// TestVerifySignature drives the §4 unit matrix. No t.Parallel(): this
// package's convention (checker.go:111-113) forbids it for tests that share
// the package's mutable, lock-free state, and the table keeps the keypairs
// local so we follow the same discipline uniformly.
func TestVerifySignature(t *testing.T) {
	pub, priv := genKey(t)
	otherPub, _ := genKey(t)
	payload := []byte("the binary bytes being verified")

	tests := []struct {
		name      string
		payload   []byte
		sig       []byte
		trustSet  []ed25519.PublicKey
		wantIndex int
		wantErr   error // sentinel via errors.Is; nil = expect success
	}{
		{
			name:      "ValidSig_Accepts",
			payload:   payload,
			sig:       signB64(priv, payload),
			trustSet:  []ed25519.PublicKey{pub},
			wantIndex: 0,
			wantErr:   nil,
		},
		{
			name:     "TamperedPayload_Rejected",
			payload:  []byte("the binary bytes being verified - tampered"),
			sig:      signB64(priv, payload),
			trustSet: []ed25519.PublicKey{pub},
			wantErr:  ErrNoTrustedKey,
		},
		{
			name:     "WrongKey_Rejected",
			payload:  payload,
			sig:      signB64(priv, payload),
			trustSet: []ed25519.PublicKey{otherPub},
			wantErr:  ErrNoTrustedKey,
		},
		{
			name:      "MultiKeyTrustSet_AnyMatchAccepts",
			payload:   payload,
			sig:       signB64(priv, payload),
			trustSet:  []ed25519.PublicKey{otherPub, pub},
			wantIndex: 1,
			wantErr:   nil,
		},
		{
			name:     "MalformedSig_BadBase64",
			payload:  payload,
			sig:      []byte("!!!not base64!!!"),
			trustSet: []ed25519.PublicKey{pub},
			wantErr:  ErrMalformedSignature,
		},
		{
			name:     "MalformedSig_ShortLength",
			payload:  payload,
			sig:      []byte(base64.StdEncoding.EncodeToString([]byte("too short"))),
			trustSet: []ed25519.PublicKey{pub},
			wantErr:  ErrMalformedSignature,
		},
		{
			name:     "MalformedSig_Empty",
			payload:  payload,
			sig:      []byte(""),
			trustSet: []ed25519.PublicKey{pub},
			wantErr:  ErrMalformedSignature,
		},
		{
			name:     "EmptyTrustSet_Rejected",
			payload:  payload,
			sig:      signB64(priv, payload),
			trustSet: nil,
			wantErr:  ErrEmptyTrustSet,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idx, err := verifySignature(tc.payload, tc.sig, tc.trustSet)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want errors.Is(_, %v)", err, tc.wantErr)
				}
				if idx != -1 {
					t.Fatalf("keyIndex = %d on error, want -1", idx)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if idx != tc.wantIndex {
				t.Fatalf("keyIndex = %d, want %d", idx, tc.wantIndex)
			}
		})
	}
}

// TestReadSigFile_MissingFile_Rejected covers the §4 MissingSigFile case:
// readSigFile on a nonexistent path returns an error, never panics.
func TestReadSigFile_MissingFile_Rejected(t *testing.T) {
	_, err := readSigFile(filepath.Join(t.TempDir(), "does-not-exist.sig"))
	if err == nil {
		t.Fatal("readSigFile on a missing path must return an error")
	}
}

// TestReadSigFile_ReadsContent confirms readSigFile returns the file bytes
// verbatim for a normal-sized signature file, and that the round-trip into
// verifySignature accepts a valid signature read from disk.
func TestReadSigFile_ReadsContent(t *testing.T) {
	pub, priv := genKey(t)
	payload := []byte("payload from disk")
	sig := signB64(priv, payload)

	dir := t.TempDir()
	sigPath := filepath.Join(dir, "asset.sig")
	if err := os.WriteFile(sigPath, sig, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readSigFile(sigPath)
	if err != nil {
		t.Fatalf("readSigFile: %v", err)
	}
	if string(got) != string(sig) {
		t.Fatalf("readSigFile content = %q, want %q", got, sig)
	}
	if _, err := verifySignature(payload, got, []ed25519.PublicKey{pub}); err != nil {
		t.Fatalf("signature read from disk should verify: %v", err)
	}
}

// TestReadSigFile_Oversized_Rejected confirms the size cap rejects a file
// larger than maxSigBytes rather than silently truncating.
func TestReadSigFile_Oversized_Rejected(t *testing.T) {
	dir := t.TempDir()
	sigPath := filepath.Join(dir, "huge.sig")
	if err := os.WriteFile(sigPath, []byte(strings.Repeat("A", maxSigBytes+10)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSigFile(sigPath); err == nil {
		t.Fatal("oversized signature file must be rejected")
	}
}

// TestTrustedSigKeysEmptyThisPhase pins the invariant that the embedded trust
// set is intentionally empty until the key-trust phase, so the unwired
// primitive hard-fails (ErrEmptyTrustSet) rather than silently passing if a
// later phase wires it in before provisioning a key.
func TestTrustedSigKeysEmptyThisPhase(t *testing.T) {
	if len(trustedSigKeys) != 0 {
		t.Fatalf("trustedSigKeys must stay empty this phase, got %d keys", len(trustedSigKeys))
	}
	if _, err := verifySignature([]byte("x"), []byte("y"), trustedSigKeys); !errors.Is(err, ErrEmptyTrustSet) {
		t.Fatalf("empty embedded trust set must hard-fail with ErrEmptyTrustSet, got %v", err)
	}
}

// TestStrictIntegrityRequested_Parsing pins the truthy/falsy parsing of the
// NAOZHI_UPGRADE_REQUIRE_PIN flag. The set of accepted truthy values is
// deliberately narrow, and any unrecognised value (including a typo) leaves
// the historical default (false) so a typo never silently disables an
// upgrade an operator did not intend to gate.
func TestStrictIntegrityRequested_Parsing(t *testing.T) {
	truthy := []string{"1", "true", "TRUE", "Yes", "on", " on ", "ON"}
	for _, v := range truthy {
		t.Setenv(requirePinEnvVar, v)
		if !strictIntegrityRequested() {
			t.Fatalf("value %q should enable strict integrity", v)
		}
	}
	falsy := []string{"", "0", "false", "no", "off", "ture", "2", "enabled?"}
	for _, v := range falsy {
		t.Setenv(requirePinEnvVar, v)
		if strictIntegrityRequested() {
			t.Fatalf("value %q must NOT enable strict integrity (default off)", v)
		}
	}
}

// TestEnforceStrongTrust_DefaultOff confirms the gate is a no-op when the
// operator has not opted into strict integrity — the load-bearing default
// for every existing operator. Empty trust set + no pin must still pass.
func TestEnforceStrongTrust_DefaultOff(t *testing.T) {
	t.Setenv(requirePinEnvVar, "")
	t.Setenv(pinSha256EnvVar, "")
	if len(trustedSigKeys) != 0 {
		t.Fatalf("precondition: trustedSigKeys must be empty this phase, got %d", len(trustedSigKeys))
	}
	if err := enforceStrongTrust(); err != nil {
		t.Fatalf("strict mode off must be a no-op, got: %v", err)
	}
}

// TestEnforceStrongTrust_StrictNoAnchor_Refused is the security-critical
// branch: strict mode on, empty embedded trust set, no out-of-band pin →
// the upgrade must hard-fail with ErrStrictNoStrongTrust rather than fall
// back to the same-channel checksums.txt that a leaked release token could
// swap in lock-step (R20260606-SEC-2 #1823).
func TestEnforceStrongTrust_StrictNoAnchor_Refused(t *testing.T) {
	t.Setenv(requirePinEnvVar, "1")
	t.Setenv(pinSha256EnvVar, "")
	if len(trustedSigKeys) != 0 {
		t.Fatalf("precondition: trustedSigKeys must be empty this phase, got %d", len(trustedSigKeys))
	}
	if err := enforceStrongTrust(); !errors.Is(err, ErrStrictNoStrongTrust) {
		t.Fatalf("strict mode with no anchor must refuse with ErrStrictNoStrongTrust, got: %v", err)
	}
}

// TestEnforceStrongTrust_StrictWithPin_Allowed confirms that a strict-mode
// operator who DID provide an out-of-band checksums pin clears the gate (the
// real pin verification still runs later in verifyPinnedChecksumsFile). A
// blank/whitespace pin does not count as an anchor.
func TestEnforceStrongTrust_StrictWithPin_Allowed(t *testing.T) {
	t.Setenv(requirePinEnvVar, "true")
	t.Setenv(pinSha256EnvVar, strings.Repeat("a", 64))
	if err := enforceStrongTrust(); err != nil {
		t.Fatalf("strict mode with a pin set should clear the gate, got: %v", err)
	}

	t.Setenv(pinSha256EnvVar, "   ")
	if err := enforceStrongTrust(); !errors.Is(err, ErrStrictNoStrongTrust) {
		t.Fatalf("whitespace-only pin must not count as an anchor, got: %v", err)
	}
}
