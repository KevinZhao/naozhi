package selfupdate

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
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
)

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
