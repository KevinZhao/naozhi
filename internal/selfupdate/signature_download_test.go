package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// The tests in this file swap the package-global trust set and
// testHTTPTransport, so none of them may run in parallel.

// withTrustSet replaces the embedded trust set for the duration of t.
func withTrustSet(t *testing.T, keys ...ed25519.PublicKey) {
	t.Helper()
	orig := trustedSigKeys
	trustedSigKeys = keys
	t.Cleanup(func() { trustedSigKeys = orig })
}

// fakeRelease serves one release's assets; a nil sig answers 404.
type fakeRelease struct {
	bin, sums, sig []byte
	sigHits        atomic.Int32
}

func checksumsFor(bin []byte) []byte {
	h := sha256.Sum256(bin)
	return fmt.Appendf(nil, "%s  %s\n", hex.EncodeToString(h[:]), assetName())
}

// fakeReleaseTag is the tag start serves; newFakeRelease signs for it.
const fakeReleaseTag = "v1.0.0"

// newFakeRelease returns a release whose checksums.txt matches bin, signed by
// priv for fakeReleaseTag (no .sig asset when priv is nil).
func newFakeRelease(bin []byte, priv ed25519.PrivateKey) *fakeRelease {
	return newFakeReleaseSignedFor(fakeReleaseTag, bin, priv)
}

// newFakeReleaseSignedFor is newFakeRelease with the signature bound to tag.
func newFakeReleaseSignedFor(tag string, bin []byte, priv ed25519.PrivateKey) *fakeRelease {
	r := &fakeRelease{bin: bin, sums: checksumsFor(bin)}
	if priv != nil {
		payload, err := SignedPayload(tag, r.sums)
		if err != nil {
			panic(err)
		}
		r.sig = signB64(priv, payload)
	}
	return r
}

// start serves r over TLS and returns the Release that points at it.
func (r *fakeRelease) start(t *testing.T) *Release {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body []byte
		switch req.URL.Path {
		case "/v1/" + assetName():
			body = r.bin
		case "/v1/checksums.txt":
			body = r.sums
		case "/v1/checksums.txt.sig":
			r.sigHits.Add(1)
			body = r.sig
		}
		if body == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(body) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	installTestTLSTransport(t, srv)
	return &Release{
		Tag:      fakeReleaseTag,
		AssetURL: srv.URL + "/v1/" + assetName(),
		SumURL:   srv.URL + "/v1/checksums.txt",
		SigURL:   srv.URL + "/v1/checksums.txt.sig",
	}
}

// assertRefusedUnexecutable fails unless Download returned an error and left
// the fetched binary non-executable.
func assertRefusedUnexecutable(t *testing.T, err error, dir string) {
	t.Helper()
	if err == nil {
		t.Fatal("Download succeeded, want a signature refusal")
	}
	if !strings.Contains(err.Error(), "verify signature") {
		t.Errorf("error %q does not name the signature step", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	info, statErr := os.Stat(filepath.Join(dir, assetName()))
	if statErr != nil {
		t.Fatalf("stat fetched binary: %v", statErr)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("refused binary has mode %o, want 0600", mode)
	}
}

func TestDownload_ValidSignature_OK(t *testing.T) {
	pub, priv := genKey(t)
	withTrustSet(t, pub)
	r := newFakeRelease([]byte("signed binary"), priv)
	dir := t.TempDir()

	binPath, err := Download(context.Background(), r.start(t), dir)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got := r.sigHits.Load(); got != 1 {
		t.Errorf("signature fetched %d times, want 1", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(binPath)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o755 {
			t.Errorf("verified binary has mode %o, want 0755", mode)
		}
	}
}

// TestDownload_ReplayedUnderNewerTag_Refused: genuinely signed files of an
// older release, served under the newest tag, must not install.
func TestDownload_ReplayedUnderNewerTag_Refused(t *testing.T) {
	pub, priv := genKey(t)
	withTrustSet(t, pub)
	r := newFakeReleaseSignedFor("v0.9.0", []byte("old signed binary"), priv)
	dir := t.TempDir()

	_, err := Download(context.Background(), r.start(t), dir)
	if !errors.Is(err, ErrNoTrustedKey) {
		t.Fatalf("Download = %v, want ErrNoTrustedKey", err)
	}
	assertRefusedUnexecutable(t, err, dir)
}

// TestDownload_SignatureOverBareChecksums_Refused: a signature over
// checksums.txt alone, without the domain and tag lines, is refused.
func TestDownload_SignatureOverBareChecksums_Refused(t *testing.T) {
	pub, priv := genKey(t)
	withTrustSet(t, pub)
	r := newFakeRelease([]byte("binary"), nil)
	r.sig = signB64(priv, r.sums)
	dir := t.TempDir()

	_, err := Download(context.Background(), r.start(t), dir)
	if !errors.Is(err, ErrNoTrustedKey) {
		t.Fatalf("Download = %v, want ErrNoTrustedKey", err)
	}
	assertRefusedUnexecutable(t, err, dir)
}

// TestDownload_SecondTrustedKey_OK: a release signed by any key of the set
// verifies, which is what lets the CI key rotate behind a backup key.
func TestDownload_SecondTrustedKey_OK(t *testing.T) {
	primary, _ := genKey(t)
	backup, backupPriv := genKey(t)
	withTrustSet(t, primary, backup)
	r := newFakeRelease([]byte("signed by the backup key"), backupPriv)

	if _, err := Download(context.Background(), r.start(t), t.TempDir()); err != nil {
		t.Fatalf("Download: %v", err)
	}
}

func TestDownload_UntrustedSignature_Refused(t *testing.T) {
	pub, _ := genKey(t)
	_, otherPriv := genKey(t)
	withTrustSet(t, pub)
	r := newFakeRelease([]byte("binary"), otherPriv)
	dir := t.TempDir()

	_, err := Download(context.Background(), r.start(t), dir)
	if !errors.Is(err, ErrNoTrustedKey) {
		t.Fatalf("got %v, want ErrNoTrustedKey", err)
	}
	assertRefusedUnexecutable(t, err, dir)
}

// TestDownload_SwappedAssets_Refused is the attack the signature exists for:
// binary and checksums.txt replaced in lock-step, so the SHA-256 chain agrees,
// while the .sig still covers the original checksums.txt.
func TestDownload_SwappedAssets_Refused(t *testing.T) {
	pub, priv := genKey(t)
	withTrustSet(t, pub)
	r := newFakeRelease([]byte("genuine binary"), priv)
	r.bin = []byte("attacker binary")
	r.sums = checksumsFor(r.bin)
	dir := t.TempDir()

	_, err := Download(context.Background(), r.start(t), dir)
	if !errors.Is(err, ErrNoTrustedKey) {
		t.Fatalf("got %v, want ErrNoTrustedKey", err)
	}
	assertRefusedUnexecutable(t, err, dir)
}

// TestDownload_SignatureCheckedBeforeChecksums: with both a bad signature and
// a binary that mismatches checksums.txt, the signature error wins, so the
// contents of an unauthenticated checksums.txt are never acted on.
func TestDownload_SignatureCheckedBeforeChecksums(t *testing.T) {
	pub, _ := genKey(t)
	_, otherPriv := genKey(t)
	withTrustSet(t, pub)
	r := newFakeRelease([]byte("binary"), otherPriv)
	r.bin = []byte("binary that matches nothing")
	dir := t.TempDir()

	_, err := Download(context.Background(), r.start(t), dir)
	if !errors.Is(err, ErrNoTrustedKey) {
		t.Fatalf("got %v, want the signature refusal before the checksum mismatch", err)
	}
}

func TestDownload_SignatureMissing_Refused(t *testing.T) {
	pub, _ := genKey(t)
	withTrustSet(t, pub)
	r := newFakeRelease([]byte("binary"), nil)
	dir := t.TempDir()

	_, err := Download(context.Background(), r.start(t), dir)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("got %v, want a refusal naming the 404", err)
	}
	assertRefusedUnexecutable(t, err, dir)
}

func TestDownload_MalformedSignature_Refused(t *testing.T) {
	pub, priv := genKey(t)
	withTrustSet(t, pub)
	r := newFakeRelease([]byte("binary"), priv)
	r.sig = r.sig[:20] // valid base64, but 15 bytes instead of 64
	dir := t.TempDir()

	_, err := Download(context.Background(), r.start(t), dir)
	if !errors.Is(err, ErrMalformedSignature) {
		t.Fatalf("got %v, want ErrMalformedSignature", err)
	}
	assertRefusedUnexecutable(t, err, dir)
}

func TestDownload_OversizedSignature_Refused(t *testing.T) {
	pub, _ := genKey(t)
	withTrustSet(t, pub)
	r := newFakeRelease([]byte("binary"), nil)
	r.sig = []byte(strings.Repeat("A", maxSigBytes+1))
	dir := t.TempDir()

	_, err := Download(context.Background(), r.start(t), dir)
	assertRefusedUnexecutable(t, err, dir)
}

func TestDownload_NoSigURL_Refused(t *testing.T) {
	pub, priv := genKey(t)
	withTrustSet(t, pub)
	r := newFakeRelease([]byte("binary"), priv)
	rel := r.start(t)
	rel.SigURL = ""
	dir := t.TempDir()

	_, err := Download(context.Background(), rel, dir)
	assertRefusedUnexecutable(t, err, dir)
	if got := r.sigHits.Load(); got != 0 {
		t.Errorf("signature fetched %d times, want 0", got)
	}
}

// TestDownload_EmptyTrustSet_NoSigFetch pins today's behaviour: with no key
// embedded, a release with or without a .sig upgrades and the .sig is never
// requested.
func TestDownload_EmptyTrustSet_NoSigFetch(t *testing.T) {
	withTrustSet(t)
	_, priv := genKey(t)
	for _, signed := range []bool{true, false} {
		var p ed25519.PrivateKey
		if signed {
			p = priv
		}
		r := newFakeRelease([]byte("binary"), p)
		if _, err := Download(context.Background(), r.start(t), t.TempDir()); err != nil {
			t.Fatalf("signed=%v: Download: %v", signed, err)
		}
		if got := r.sigHits.Load(); got != 0 {
			t.Errorf("signed=%v: signature fetched %d times with an empty trust set, want 0", signed, got)
		}
	}
}

// TestDownload_StrictModeWithKeys_VerifiesSignature: enforceStrongTrust
// accepts an embedded key as the strict-mode anchor, so Download must then
// actually check the signature rather than pass on the key's presence.
func TestDownload_StrictModeWithKeys_VerifiesSignature(t *testing.T) {
	t.Setenv(requirePinEnvVar, "1")
	t.Setenv(pinSha256EnvVar, "")
	pub, priv := genKey(t)
	_, otherPriv := genKey(t)
	withTrustSet(t, pub)

	bad := newFakeRelease([]byte("binary"), otherPriv)
	dir := t.TempDir()
	_, err := Download(context.Background(), bad.start(t), dir)
	if !errors.Is(err, ErrNoTrustedKey) {
		t.Fatalf("strict mode, untrusted signature: got %v, want ErrNoTrustedKey", err)
	}
	assertRefusedUnexecutable(t, err, dir)

	good := newFakeRelease([]byte("binary"), priv)
	if _, err := Download(context.Background(), good.start(t), t.TempDir()); err != nil {
		t.Fatalf("strict mode, trusted signature: %v", err)
	}
}

// redirectingTransport answers github.com the way /releases/latest does.
type redirectingTransport struct{}

func (redirectingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: req}
	if strings.HasSuffix(req.URL.Path, "/releases/latest") {
		resp.StatusCode = http.StatusFound
		resp.Header.Set("Location", "https://github.com/"+repo+"/releases/tag/v1.2.3")
	}
	return resp, nil
}

func TestLatestRelease_SetsSigURL(t *testing.T) {
	if checkPlatform() != nil {
		t.Skip("no release asset on this platform")
	}
	prev := testHTTPTransport
	testHTTPTransport = redirectingTransport{}
	t.Cleanup(func() { testHTTPTransport = prev })

	rel, err := LatestRelease(context.Background())
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	want := "https://github.com/" + repo + "/releases/download/v1.2.3/checksums.txt.sig"
	if rel.SigURL != want {
		t.Errorf("SigURL = %q, want %q", rel.SigURL, want)
	}
	if strings.TrimSuffix(rel.SigURL, ".sig") != rel.SumURL {
		t.Errorf("SigURL %q is not SumURL %q + .sig", rel.SigURL, rel.SumURL)
	}
}
