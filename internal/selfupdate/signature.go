package selfupdate

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// maxSigBytes caps a signature file (an ed25519 signature is ~88 base64 chars).
const maxSigBytes = 4 * 1024 // 4 KB

// trustedSigKeys is the ed25519 trust set. Intentionally empty until the
// key-trust RFC embeds a reviewed key; while empty Download skips the
// signature step and verifySignature hard-fails (ErrEmptyTrustSet).
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
// for real by verifyReleaseSignature / verifyPinnedChecksumsFile) or fails
// with ErrStrictNoStrongTrust.
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
// ErrMalformedSignature / ErrNoTrustedKey otherwise).
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

// TrustedSigKeys returns a deep copy of the embedded trust set, so the
// release-sign tool checks a release against exactly what clients embed.
func TrustedSigKeys() []ed25519.PublicKey {
	out := make([]ed25519.PublicKey, len(trustedSigKeys))
	for i, k := range trustedSigKeys {
		out[i] = slices.Clone(k)
	}
	return out
}

// signedPayloadDomain opens every signed payload; it keeps the release key's
// signatures from meaning anything else and leaves room for a v2 format.
const signedPayloadDomain = "naozhi-release-v1\n"

// SignedPayload is the one definition of the bytes a release signature covers:
// the domain line, "tag <tag>\n", then checksums.txt as published. Binding the
// tag stops an older signed release being replayed under a newer tag. A tag
// outside tagAllowedRe is rejected, so the signer and the client accept the
// same tags and a tag can never carry a newline into the payload.
func SignedPayload(tag string, checksums []byte) ([]byte, error) {
	if !tagAllowedRe.MatchString(tag) {
		return nil, fmt.Errorf("selfupdate: release tag %q is not a valid tag", tag)
	}
	out := make([]byte, 0, len(signedPayloadDomain)+len("tag \n")+len(tag)+len(checksums))
	out = append(out, signedPayloadDomain...)
	out = append(out, "tag "...)
	out = append(out, tag...)
	out = append(out, '\n')
	return append(out, checksums...), nil
}

// VerifyReleaseSignature checks sig over SignedPayload(tag, checksums) against
// trustSet with the same decoding and sentinels the upgrade path uses, so the
// release-sign tool cannot check different bytes than deployed clients do.
func VerifyReleaseSignature(tag string, checksums, sig []byte, trustSet []ed25519.PublicKey) (keyIndex int, err error) {
	payload, err := SignedPayload(tag, checksums)
	if err != nil {
		return -1, err
	}
	return verifySignature(payload, sig, trustSet)
}

// verifyReleaseSignature fetches rel.SigURL through fetchFile's guards and
// requires it to verify SignedPayload(rel.Tag, checksums.txt) against the
// embedded trust set. Every failure is fatal, a missing .sig included: a
// fallback to the unsigned chain would let an attacker downgrade the check by
// deleting the asset.
func verifyReleaseSignature(ctx context.Context, rel *Release, dir, sumPath string) error {
	if rel.SigURL == "" {
		return fmt.Errorf("verify signature: release %s has no signature URL", rel.Tag)
	}
	sigPath := filepath.Join(dir, "checksums.txt.sig")
	if err := fetchFile(ctx, rel.SigURL, sigPath, maxSigBytes); err != nil {
		return fmt.Errorf("verify signature: download: %w", err)
	}
	sig, err := readSigFile(sigPath)
	if err != nil {
		return fmt.Errorf("verify signature: %w", err)
	}
	sums, err := os.ReadFile(sumPath)
	if err != nil {
		return fmt.Errorf("verify signature: read checksums: %w", err)
	}
	idx, err := VerifyReleaseSignature(rel.Tag, sums, sig, trustedSigKeys)
	if err != nil {
		return fmt.Errorf("verify signature: %w", err)
	}
	slog.Info("selfupdate: checksums signature verified", "tag", rel.Tag, "key_index", idx)
	return nil
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
