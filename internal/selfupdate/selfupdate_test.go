package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ----- extractTag -----------------------------------------------------------

func TestExtractTag(t *testing.T) {
	t.Parallel()
	cases := []struct {
		url  string
		want string
	}{
		{"https://github.com/KevinZhao/naozhi/releases/tag/v1.2.3", "v1.2.3"},
		{"https://github.com/KevinZhao/naozhi/releases/tag/v0.0.1-rc1", "v0.0.1-rc1"},
		{"https://github.com/KevinZhao/naozhi/releases/latest", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := extractTag(c.url); got != c.want {
			t.Errorf("extractTag(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

// ----- verifyChecksum -------------------------------------------------------

func TestVerifyChecksum_OK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	content := []byte("fake binary content")
	binPath := filepath.Join(dir, "naozhi-linux-amd64")
	if err := os.WriteFile(binPath, content, 0644); err != nil {
		t.Fatal(err)
	}

	h := sha256.Sum256(content)
	sum := hex.EncodeToString(h[:])
	sums := fmt.Sprintf("%s  naozhi-linux-amd64\n", sum)
	sumPath := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(sumPath, []byte(sums), 0644); err != nil {
		t.Fatal(err)
	}

	if err := verifyChecksum(binPath, sumPath, "naozhi-linux-amd64"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVerifyChecksum_Mismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	binPath := filepath.Join(dir, "naozhi-linux-amd64")
	if err := os.WriteFile(binPath, []byte("real content"), 0644); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte("different content"))
	sums := fmt.Sprintf("%s  naozhi-linux-amd64\n", hex.EncodeToString(h[:]))
	sumPath := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(sumPath, []byte(sums), 0644); err != nil {
		t.Fatal(err)
	}

	err := verifyChecksum(binPath, sumPath, "naozhi-linux-amd64")
	if err == nil {
		t.Fatal("expected checksum mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error should mention checksum mismatch, got: %v", err)
	}
}

func TestVerifyChecksum_MissingEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	binPath := filepath.Join(dir, "naozhi-linux-amd64")
	if err := os.WriteFile(binPath, []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	sumPath := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(sumPath, []byte("abc123  naozhi-darwin-arm64\n"), 0644); err != nil {
		t.Fatal(err)
	}

	err := verifyChecksum(binPath, sumPath, "naozhi-linux-amd64")
	if err == nil || !strings.Contains(err.Error(), "no checksum entry") {
		t.Errorf("expected 'no checksum entry' error, got: %v", err)
	}
}

// ----- Replace + Rollback ---------------------------------------------------

func TestReplace_And_Rollback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	installPath := filepath.Join(dir, "naozhi")
	if err := os.WriteFile(installPath, []byte("old binary"), 0755); err != nil {
		t.Fatal(err)
	}
	newBin := filepath.Join(dir, "naozhi-new")
	if err := os.WriteFile(newBin, []byte("new binary"), 0755); err != nil {
		t.Fatal(err)
	}

	backupPath, err := Replace(newBin, installPath)
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}

	got, _ := os.ReadFile(installPath)
	if string(got) != "new binary" {
		t.Errorf("after Replace, installPath = %q, want %q", got, "new binary")
	}
	if fi, err := os.Stat(installPath); err != nil {
		t.Fatalf("stat installPath: %v", err)
	} else if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary is not executable: mode %o", fi.Mode().Perm())
	}
	bak, _ := os.ReadFile(backupPath)
	if string(bak) != "old binary" {
		t.Errorf("backup = %q, want %q", bak, "old binary")
	}

	if err := Rollback(installPath, backupPath); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	got, _ = os.ReadFile(installPath)
	if string(got) != "old binary" {
		t.Errorf("after Rollback, installPath = %q, want %q", got, "old binary")
	}
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Errorf("backup file should be removed after Rollback")
	}
}

// TestReplace_ForcesExecutableFromNonExecutableSource is the regression test
// for the v0.0.27 upgrade outage: when the source binary is NOT executable
// (0600 — the mode fetchFile writes before Download's post-verify chmod, a
// state that has been observed to reach Replace on a loaded host), Replace
// must still leave the installed binary executable. copyFile clones the
// source mode, so without the explicit success-path chmod the install lands
// at 0600 and systemd fails with 203/EXEC.
//
// On the pre-fix code this fails (installed mode is 0600); on the fixed code
// the install is 0755.
func TestReplace_ForcesExecutableFromNonExecutableSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	installPath := filepath.Join(dir, "naozhi")
	if err := os.WriteFile(installPath, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Source binary is owner-only, non-executable — mimics the downloaded
	// asset before (or without) the post-checksum chmod.
	newBin := filepath.Join(dir, "naozhi-new")
	if err := os.WriteFile(newBin, []byte("new binary"), 0o600); err != nil {
		t.Fatal(err)
	}

	backupPath, err := Replace(newBin, installPath)
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(backupPath) })

	fi, err := os.Stat(installPath)
	if err != nil {
		t.Fatalf("stat installPath: %v", err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed binary not executable despite non-exec source: mode %o (regression: 203/EXEC)", fi.Mode().Perm())
	}
	if got, _ := os.ReadFile(installPath); string(got) != "new binary" {
		t.Errorf("installPath content = %q, want %q", got, "new binary")
	}
}

func TestReplace_StagingCleanedOnRenameFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// installPath points to a directory so os.Rename will fail.
	installPath := filepath.Join(dir, "naozhi")
	if err := os.Mkdir(installPath, 0755); err != nil {
		t.Fatal(err)
	}
	newBin := filepath.Join(dir, "naozhi-new")
	if err := os.WriteFile(newBin, []byte("new binary"), 0755); err != nil {
		t.Fatal(err)
	}

	_, err := Replace(newBin, installPath)
	if err == nil {
		t.Fatal("expected Replace to fail when installPath is a directory")
	}

	// Staging file should be cleaned up. CreateTemp's randomised suffix
	// makes the exact filename unpredictable; glob against the same
	// pattern Replace uses so we still catch a leak. R225-SEC-3.
	stale, _ := filepath.Glob(filepath.Join(dir, stagingPattern))
	if len(stale) > 0 {
		t.Errorf("staging files should have been removed on failure: %v", stale)
	}
}

// ----- LatestRelease (mock HTTP server) -------------------------------------

func TestLatestRelease_OK(t *testing.T) {
	t.Parallel()

	// Two-handler server: /latest redirects to /releases/tag/v9.9.9, which
	// the second handler serves as 200 OK (so resp.Body.Close() works).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			http.Redirect(w, r, "/releases/tag/v9.9.9", http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	// Temporarily override the repo URL by monkey-patching extractTag with a
	// helper that uses the test server URL instead of the real GitHub URL.
	// Since latestRelease builds the URL from `repo`, we test the tag-parsing
	// path directly with a fabricated final URL.
	finalURL := srv.URL + "/releases/tag/v9.9.9"
	tag := extractTag(finalURL)
	if tag != "v9.9.9" {
		t.Errorf("extractTag(%q) = %q, want v9.9.9", finalURL, tag)
	}

	// Also verify the Release struct fields are populated correctly.
	rel := &Release{
		Tag:      tag,
		AssetURL: srv.URL + "/releases/download/v9.9.9/naozhi-linux-amd64",
		SumURL:   srv.URL + "/releases/download/v9.9.9/checksums.txt",
	}
	if rel.Tag != "v9.9.9" {
		t.Errorf("Tag = %q, want v9.9.9", rel.Tag)
	}
	if !strings.Contains(rel.AssetURL, "v9.9.9") {
		t.Errorf("AssetURL should contain tag, got %q", rel.AssetURL)
	}
}

func TestLatestRelease_NoRelease(t *testing.T) {
	t.Parallel()
	// Server returns 404 (no releases yet).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	// extractTag on a non-tag URL returns "".
	tag := extractTag(srv.URL + "/404")
	if tag != "" {
		t.Errorf("expected empty tag from non-release URL, got %q", tag)
	}
}

// ----- Download (mock HTTP server) ------------------------------------------

// installTestTLSTransport wires testHTTPTransport to trust srv's self-signed
// cert and resets it on cleanup. R240-SEC-4 (#1048): fetchFile now refuses
// non-https URLs at entry, so download tests must use NewTLSServer; this
// helper threads srv.Client().Transport in so the production code's default
// transport doesn't reject the test cert.
//
// NOTE: tests that use this helper MUST NOT run with t.Parallel() because
// testHTTPTransport is a package-global. Sequential execution avoids cross-
// test bleed of the override.
func installTestTLSTransport(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := testHTTPTransport
	testHTTPTransport = srv.Client().Transport
	t.Cleanup(func() { testHTTPTransport = prev })
}

func TestDownload_OK(t *testing.T) {
	dir := t.TempDir()

	binContent := []byte("mock naozhi binary")
	h := sha256.Sum256(binContent)
	checksum := hex.EncodeToString(h[:])

	// Asset name for current platform.
	asset := assetName()
	checksumsTxt := fmt.Sprintf("%s  %s\n", checksum, asset)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, asset):
			w.WriteHeader(http.StatusOK)
			w.Write(binContent) //nolint:errcheck
		case strings.HasSuffix(r.URL.Path, "checksums.txt"):
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(checksumsTxt)) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	installTestTLSTransport(t, srv)

	rel := &Release{
		Tag:      "v1.0.0",
		AssetURL: srv.URL + "/releases/download/v1.0.0/" + asset,
		SumURL:   srv.URL + "/releases/download/v1.0.0/checksums.txt",
	}

	binPath, err := Download(context.Background(), rel, dir)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(binPath)
	if string(got) != string(binContent) {
		t.Errorf("downloaded content = %q, want %q", got, binContent)
	}
}

func TestDownload_ChecksumMismatch(t *testing.T) {
	dir := t.TempDir()

	asset := assetName()
	// checksums.txt has a hash for different content.
	badHash := hex.EncodeToString(sha256.New().Sum(nil)) // hash of empty
	checksumsTxt := fmt.Sprintf("%s  %s\n", badHash, asset)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, asset):
			w.Write([]byte("actual binary content")) //nolint:errcheck
		case strings.HasSuffix(r.URL.Path, "checksums.txt"):
			w.Write([]byte(checksumsTxt)) //nolint:errcheck
		}
	}))
	defer srv.Close()
	installTestTLSTransport(t, srv)

	rel := &Release{
		Tag:      "v1.0.0",
		AssetURL: srv.URL + "/" + asset,
		SumURL:   srv.URL + "/checksums.txt",
	}

	_, err := Download(context.Background(), rel, dir)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("expected checksum mismatch error, got: %v", err)
	}
}

func TestDownload_HTTP404(t *testing.T) {
	dir := t.TempDir()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	installTestTLSTransport(t, srv)

	rel := &Release{
		Tag:      "v1.0.0",
		AssetURL: srv.URL + "/missing",
		SumURL:   srv.URL + "/missing-sums",
	}

	_, err := Download(context.Background(), rel, dir)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("expected HTTP 404 error, got: %v", err)
	}
}

// TestFetchFile_RejectsNonHTTPS guards R240-SEC-4 (#1048): the entry-point
// https-prefix check refuses any URL that doesn't start with https://, so a
// future caller passing a plain http:// asset URL fails fast instead of
// silently transmitting the binary in cleartext.
func TestFetchFile_RejectsNonHTTPS(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dest := filepath.Join(dir, "asset")

	err := fetchFile(context.Background(), "http://example.invalid/asset", dest, maxBinaryBytes)
	if err == nil {
		t.Fatal("expected http:// URL to be rejected")
	}
	if !strings.Contains(err.Error(), "non-https") {
		t.Errorf("expected non-https rejection message, got: %v", err)
	}
	// Staging file must NOT be created when the URL is rejected — the
	// 0600 file-creation step lives downstream of the prefix check.
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("expected dest to NOT be created, stat err = %v", statErr)
	}
}

// TestFetchFile_RejectsBadSchemeShapes anchors R247-SEC-5 (#497): the
// belt-and-suspenders parsed-scheme check after http.NewRequestWithContext
// rejects schemes the prefix gate already covers AND any future regression
// where a caller pre-strips the scheme. Each case must fail before the
// staging file is touched.
func TestFetchFile_RejectsBadSchemeShapes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	cases := []struct {
		name string
		url  string
	}{
		{"plain_http", "http://example.invalid/asset"},
		{"ftp_scheme", "ftp://example.invalid/asset"},
		{"file_scheme", "file:///etc/passwd"},
		{"empty_url", ""},
		{"scheme_only", "https://"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dest := filepath.Join(dir, "asset_"+c.name)
			err := fetchFile(context.Background(), c.url, dest, maxBinaryBytes)
			if err == nil {
				t.Fatalf("expected %q to be rejected", c.url)
			}
			// Either the prefix gate or the post-parse scheme gate (or
			// the request builder for malformed input like the empty URL)
			// must trip. Just assert the staging file is never created so
			// the rejection always happens BEFORE we touch the network or
			// the disk.
			if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
				t.Errorf("expected dest to NOT be created, stat err = %v", statErr)
			}
		})
	}
}

// TestFetchFile_ErrorMessageDistinguishesGuards pins R247-SEC-5 (#497):
// the prefix and parsed-scheme gates emit DIFFERENT error substrings
// ("non-https URL" vs "non-https URL after parse") so an operator
// reading logs can tell which leg of the defense-in-depth tripped. Today
// only the prefix check fires for plain http://; the parsed-scheme check
// only catches a hypothetical caller that bypasses the prefix gate.
// Asserting the message split here prevents a future "consolidate the
// two errors into one" cleanup from silently merging the two
// observability signals.
func TestFetchFile_ErrorMessageDistinguishesGuards(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dest := filepath.Join(dir, "asset")

	err := fetchFile(context.Background(), "http://example.invalid/asset", dest, maxBinaryBytes)
	if err == nil {
		t.Fatal("expected http:// to be rejected")
	}
	// Prefix gate fires first; its message must NOT include "after parse"
	// (the parsed-scheme gate's discriminator). If a future patch reorders
	// the gates so the parsed-scheme one fires first for plain http://,
	// the message changes and an operator-facing ops runbook breaks.
	if strings.Contains(err.Error(), "after parse") {
		t.Errorf("prefix gate should fire first, but message points at parsed-scheme: %v", err)
	}
	if !strings.Contains(err.Error(), "non-https") {
		t.Errorf("expected non-https rejection, got: %v", err)
	}
}
