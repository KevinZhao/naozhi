package selfupdate

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// maxSigBytes caps the size of a signature file. An ed25519 signature is 64
// raw bytes (~88 base64 chars), so this is a generous upper bound that still
// rejects a hostile mirror serving a giant file to exhaust memory. Mirrors
// the maxChecksumBytes hardening rationale.
const maxSigBytes = 4 * 1024 // 4 KB

// trustedSigKeys is the set of ed25519 public keys the verifier trusts.
//
// It is intentionally left empty until the key-trust RFC phase embeds a
// reviewed public key. While empty, verifySignature hard-fails (see
// ErrEmptyTrustSet) so this primitive can never silently pass if a later
// phase wires it in before a key is provisioned.
var trustedSigKeys []ed25519.PublicKey

// Signature-verification error sentinels. Distinct values let callers (and
// tests) distinguish a malformed signature from an empty trust set from a
// genuine no-match, without string matching.
var (
	// ErrEmptyTrustSet is returned when verifySignature is called with no
	// trusted keys. This is the safety net that keeps the unwired primitive
	// from passing if mis-wired before a key is embedded.
	ErrEmptyTrustSet = errors.New("selfupdate: empty trust set — refusing to verify signature")

	// ErrMalformedSignature is returned when the signature is not valid
	// base64 or decodes to the wrong length for an ed25519 signature.
	ErrMalformedSignature = errors.New("selfupdate: malformed signature")

	// ErrNoTrustedKey is returned when no key in the trust set verifies the
	// signature over the payload.
	ErrNoTrustedKey = errors.New("selfupdate: no trusted key verified signature")

	// ErrStrictNoStrongTrust is returned when the operator opts into strict
	// integrity (NAOZHI_UPGRADE_REQUIRE_PIN) but no strong-trust anchor is
	// available — i.e. the embedded ed25519 trust set is empty AND no
	// out-of-band checksums.txt pin (NAOZHI_UPGRADE_PIN_SHA256) is set. In
	// that state the only remaining integrity guarantee is the same-channel
	// checksums.txt, which a leaked GitHub release token can swap in lock-step
	// with the binary (R20260606-SEC-2 #1823). Strict mode refuses to upgrade
	// rather than silently relying on that weak chain.
	ErrStrictNoStrongTrust = errors.New("selfupdate: strict integrity required but no signing key embedded and no out-of-band checksums pin set — refusing upgrade")
)

// requirePinEnvVar names the env var an operator sets to demand a strong
// integrity anchor before any self-update is applied. Truthy values:
// "1", "true", "yes", "on" (case-insensitive). Anything else (including
// unset/empty) leaves the default best-effort behaviour unchanged.
//
// R20260606-SEC-2 (#1823): the embedded ed25519 trust set is intentionally
// empty pending the key-trust RFC, so the only production integrity check is
// the SHA-256 chain to checksums.txt — fetched over the SAME GitHub release
// channel as the binary. A leaked release-write token swaps both files
// together and the chain still verifies. Embedding a real key needs a
// private-key/key-management decision out of scope for a code-only fix; until
// then this flag lets a security-conscious operator turn the silent
// same-channel fallback into a hard failure: with the flag set, an upgrade
// proceeds ONLY when a strong anchor exists (a non-empty embedded trust set,
// once provisioned, or an operator-set NAOZHI_UPGRADE_PIN_SHA256 pin).
const requirePinEnvVar = "NAOZHI_UPGRADE_REQUIRE_PIN"

// strictIntegrityRequested reports whether the operator opted into strict
// integrity via requirePinEnvVar. Parsing is intentionally narrow so a typo
// ("ture") does not accidentally enable a mode the operator believes is off
// — but more importantly a typo never silently DISABLES protection either,
// because the default (this returning false) is the historical behaviour.
func strictIntegrityRequested() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(requirePinEnvVar))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// enforceStrongTrust is the Download-time gate for strict integrity. It is a
// no-op unless the operator set requirePinEnvVar. When strict mode is on it
// requires at least one strong-trust anchor:
//
//   - a non-empty embedded ed25519 trust set (verifySignature can then run
//     against a real key once the key-trust phase provisions one), OR
//   - a syntactically valid NAOZHI_UPGRADE_PIN_SHA256 pin (verified for real
//     against checksums.txt by verifyPinnedChecksumsFile in the same flow).
//
// pinSet must reflect whether a non-empty pin env var is present (the caller
// already reads it for verifyPinnedChecksumsFile; we re-read here to keep this
// gate self-contained and independently testable). With neither anchor present
// strict mode hard-fails with ErrStrictNoStrongTrust instead of proceeding on
// the same-channel checksums.txt alone.
func enforceStrongTrust() error {
	if !strictIntegrityRequested() {
		return nil
	}
	if len(trustedSigKeys) > 0 {
		return nil
	}
	if strings.TrimSpace(os.Getenv(pinSha256EnvVar)) != "" {
		return nil
	}
	return ErrStrictNoStrongTrust
}

// verifySignature checks sig (base64-encoded ed25519 signature) against
// payload using each key in trustSet, returning the index of the first key
// that verifies. It never panics on malformed input.
//
// Errors:
//   - ErrEmptyTrustSet  when trustSet is empty (no-op guard).
//   - ErrMalformedSignature when sig is empty, not valid base64, or decodes
//     to a length other than ed25519.SignatureSize.
//   - ErrNoTrustedKey when no key verifies the signature.
//
// This primitive is unwired this phase: nothing in production calls it until
// the key-trust phase embeds a reviewed public key into trustedSigKeys.
func verifySignature(payload, sig []byte, trustSet []ed25519.PublicKey) (keyIndex int, err error) {
	if len(trustSet) == 0 {
		return -1, ErrEmptyTrustSet
	}
	if len(sig) == 0 {
		return -1, fmt.Errorf("%w: empty signature", ErrMalformedSignature)
	}
	raw, err := base64.StdEncoding.DecodeString(string(sig))
	if err != nil {
		return -1, fmt.Errorf("%w: base64 decode: %v", ErrMalformedSignature, err)
	}
	if len(raw) != ed25519.SignatureSize {
		return -1, fmt.Errorf("%w: decoded length %d, want %d", ErrMalformedSignature, len(raw), ed25519.SignatureSize)
	}
	for i, pub := range trustSet {
		if ed25519.Verify(pub, payload, raw) {
			return i, nil
		}
	}
	return -1, ErrNoTrustedKey
}

// readSigFile reads a signature file with a small size cap, mirroring the
// maxChecksumBytes guard on checksums.txt. Returns an error rather than
// panicking on a missing or oversized file.
func readSigFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: open signature file %s: %w", path, err)
	}
	defer f.Close()

	// Read up to maxSigBytes+1 so an oversized file is detected rather than
	// silently truncated (which would later surface as a confusing decode
	// or verify failure instead of the real cause).
	data, err := io.ReadAll(io.LimitReader(f, maxSigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("selfupdate: read signature file %s: %w", path, err)
	}
	if int64(len(data)) > maxSigBytes {
		return nil, fmt.Errorf("selfupdate: signature file %s exceeds %d bytes", path, maxSigBytes)
	}
	return data, nil
}
