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

// maxSigBytes caps a signature file (an ed25519 signature is ~88 base64 chars).
const maxSigBytes = 4 * 1024 // 4 KB

// trustedSigKeys is the ed25519 trust set. Intentionally empty until the
// key-trust RFC embeds a reviewed key; while empty verifySignature hard-fails
// (ErrEmptyTrustSet) so the primitive can never silently pass.
var trustedSigKeys []ed25519.PublicKey

// Signature-verification sentinels, distinct so callers need no string matching.
var (
	// ErrEmptyTrustSet: verifySignature called with no trusted keys.
	ErrEmptyTrustSet = errors.New("selfupdate: empty trust set — refusing to verify signature")

	// ErrMalformedSignature: not valid base64 or wrong length for ed25519.
	ErrMalformedSignature = errors.New("selfupdate: malformed signature")

	// ErrNoTrustedKey: no key in the trust set verifies the payload.
	ErrNoTrustedKey = errors.New("selfupdate: no trusted key verified signature")

	// ErrStrictNoStrongTrust: strict integrity requested but neither an
	// embedded key nor an out-of-band checksums pin exists, leaving only the
	// same-channel checksums.txt a leaked release token can swap (#1823).
	ErrStrictNoStrongTrust = errors.New("selfupdate: strict integrity required but no signing key embedded and no out-of-band checksums pin set — refusing upgrade")
)

// requirePinEnvVar ("1"/"true"/"yes"/"on") demands a strong integrity anchor
// before any self-update: with the trust set empty pending the key-trust RFC,
// the only production check is the same-channel SHA-256 chain, which a leaked
// release token defeats. Set, an upgrade proceeds only with an embedded key or
// a NAOZHI_UPGRADE_PIN_SHA256 pin (#1823).
const requirePinEnvVar = "NAOZHI_UPGRADE_REQUIRE_PIN"

// strictIntegrityRequested reports whether the operator opted into strict
// integrity. Parsing is narrow: a typo never silently changes the mode.
func strictIntegrityRequested() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(requirePinEnvVar))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// enforceStrongTrust is the Download-time gate: a no-op unless strict mode is
// requested, then requires an embedded trust set or a checksums pin (verified
// for real by verifyPinnedChecksumsFile) or fails with ErrStrictNoStrongTrust.
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

// verifySignature checks a base64 ed25519 sig against payload with each key in
// trustSet and returns the index of the first that verifies (ErrEmptyTrustSet /
// ErrMalformedSignature / ErrNoTrustedKey otherwise). Unwired in production
// until a reviewed key is embedded.
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

// readSigFile reads a signature file with a small size cap.
func readSigFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: open signature file %s: %w", path, err)
	}
	defer f.Close()

	// maxSigBytes+1 detects oversize instead of silently truncating.
	data, err := io.ReadAll(io.LimitReader(f, maxSigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("selfupdate: read signature file %s: %w", path, err)
	}
	if int64(len(data)) > maxSigBytes {
		return nil, fmt.Errorf("selfupdate: signature file %s exceeds %d bytes", path, maxSigBytes)
	}
	return data, nil
}
