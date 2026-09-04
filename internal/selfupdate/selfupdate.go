// Package selfupdate fetches a newer naozhi binary from GitHub Releases,
// verifies its SHA-256 checksum, atomically replaces the running binary,
// and optionally restarts the system service.
//
// Flow:
//
//	LatestRelease()     → GitHub redirect → semver tag
//	Download()          → binary + checksums.txt → tmp dir
//	Replace()           → backup current, rename new binary into place
//	RestartService()    → systemctl restart / launchctl reload
package selfupdate

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	repo           = "KevinZhao/naozhi"
	defaultTimeout = 60 * time.Second
	backupSuffix   = ".naozhi-upgrade.bak"

	// maxBinaryBytes caps the download so a rogue asset cannot fill the disk.
	maxBinaryBytes = 200 * 1024 * 1024 // 200 MB

	// maxChecksumBytes caps checksums.txt (legit manifests are a few KB).
	maxChecksumBytes = 64 * 1024 // 64 KB
)

// ErrUnsupportedPlatform is returned when the current OS has no release asset.
var ErrUnsupportedPlatform = errors.New("upgrade not supported on this platform (no release asset)")

// Release holds metadata about a GitHub Release.
type Release struct {
	Tag      string // e.g. "v1.2.3"
	AssetURL string // direct binary URL
	SumURL   string // checksums.txt URL
}

// LatestRelease resolves the latest release tag by following the
// /releases/latest redirect (anonymous, no API token).
func LatestRelease(ctx context.Context) (*Release, error) {
	if err := checkPlatform(); err != nil {
		return nil, fmt.Errorf("check platform: %w", err)
	}

	latestURL := fmt.Sprintf("https://github.com/%s/releases/latest", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// Only the final redirect URL matters. CheckRedirect pins every hop to
	// github.com so a hostile CDN/DNS cannot feed extractTag an attacker tag;
	// the DialContext guard rejects reserved IPs after resolution (DNS
	// rebinding to IMDS). Test mode uses testHTTPTransport unguarded.
	var latestTransport http.RoundTripper
	if dialCtx := blockPrivateDialContext(); dialCtx != nil {
		latestTransport = hardenedTransport(dialCtx)
	} else {
		latestTransport = testHTTPTransport // nil in production falls back to http.DefaultTransport
	}
	client := &http.Client{
		Timeout:   defaultTimeout,
		Transport: latestTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 5 {
				return fmt.Errorf("too many redirects")
			}
			if !isGitHubHost(req.URL.Host) {
				return fmt.Errorf("redirect target outside github.com: %s", req.URL.Host)
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch latest release URL: %w", err)
	}
	resp.Body.Close()

	// Final URL shape: .../releases/tag/v1.2.3
	final := resp.Request.URL.String()
	tag := extractTag(final)
	if tag == "" {
		return nil, fmt.Errorf("could not parse release tag from URL %q", final)
	}

	asset := assetName()
	// PathEscape so a tag that slipped `?x=y` or separators past the extractor
	// cannot pivot the download URL.
	base := fmt.Sprintf("https://github.com/%s/releases/download/%s", repo, url.PathEscape(tag))
	return &Release{
		Tag:      tag,
		AssetURL: base + "/" + asset,
		SumURL:   base + "/checksums.txt",
	}, nil
}

// pinSha256EnvVar lets an operator pin the expected SHA-256 of checksums.txt
// itself (recorded out-of-band). The binary checksum alone is no stronger than
// the GitHub release token — a leaked token swaps BOTH files in lock-step — so
// the pin adds an anchor the token cannot reach. Unset = best-effort chain;
// a signed release flow (cosign / Sigstore) is the long-term fix (#815).
const pinSha256EnvVar = "NAOZHI_UPGRADE_PIN_SHA256"

// pinSha256HexRe rejects a malformed pin early (case-insensitive so uppercase
// hex from another tool still works).
var pinSha256HexRe = regexp.MustCompile(`^[A-Fa-f0-9]{64}$`)

// Download fetches the binary and checksums.txt into dir, verifies the SHA-256
// and returns the binary path. The file stays 0600 (non-executable) until
// verification succeeds so its mode never claims "ready to execute" early.
// With NAOZHI_UPGRADE_PIN_SHA256 set, checksums.txt must match the pin before
// it is trusted (#815).
func Download(ctx context.Context, rel *Release, dir string) (binPath string, err error) {
	// Strict mode with no strong-trust anchor refuses before any network I/O (#1823).
	if err := enforceStrongTrust(); err != nil {
		return "", err
	}

	asset := assetName()
	binPath = filepath.Join(dir, asset)

	if err := fetchFile(ctx, rel.AssetURL, binPath, maxBinaryBytes); err != nil {
		return "", fmt.Errorf("download binary: %w", err)
	}
	sumPath := filepath.Join(dir, "checksums.txt")
	if err := fetchFile(ctx, rel.SumURL, sumPath, maxChecksumBytes); err != nil {
		return "", fmt.Errorf("download checksums: %w", err)
	}
	// Pin check BEFORE the chain: a tampered checksums.txt would happily
	// verify a tampered binary.
	if err := verifyPinnedChecksumsFile(sumPath); err != nil {
		return "", err
	}
	if err := verifyChecksum(binPath, sumPath, asset); err != nil {
		return "", err
	}
	// Verified: now executable.
	if err := os.Chmod(binPath, 0o755); err != nil {
		return "", fmt.Errorf("chmod verified binary: %w", err)
	}
	return binPath, nil
}

// verifyPinnedChecksumsFile enforces the optional checksums.txt pin; nil when
// unset. A malformed pin errors rather than silently bypassing a pin the
// operator believes is active.
func verifyPinnedChecksumsFile(sumPath string) error {
	pin := strings.TrimSpace(os.Getenv(pinSha256EnvVar))
	if pin == "" {
		return nil
	}
	if !pinSha256HexRe.MatchString(pin) {
		return fmt.Errorf("selfupdate: %s set but not a 64-char hex SHA-256: %q", pinSha256EnvVar, pin)
	}
	data, err := os.ReadFile(sumPath)
	if err != nil {
		return fmt.Errorf("selfupdate: read checksums.txt for pin verify: %w", err)
	}
	h := sha256.Sum256(data)
	actual := hex.EncodeToString(h[:])
	expected := strings.ToLower(pin)
	if subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
		return fmt.Errorf("selfupdate: checksums.txt SHA-256 %s does not match pinned %s — refusing upgrade",
			actual, expected)
	}
	return nil
}

// stagingPattern is Replace's os.CreateTemp pattern; package-level so tests
// can glob for stale staging files.
const stagingPattern = ".naozhi-upgrade-*.staging"

// Replace atomically swaps newBin into installPath: back up the current binary
// to installPath+backupSuffix, write newBin to an O_EXCL random-suffix staging
// file in the same directory (a fixed name would give a hostile UID on a
// shared install dir a pre-creatable target), os.Rename into place, and
// restore the backup on any failure after the backup was taken.
func Replace(newBin, installPath string) (backupPath string, err error) {
	backupPath = installPath + backupSuffix

	// Backup is forced 0600: on a shared install dir a 0755 copy of the prior
	// version would be readable/executable by other UIDs during the upgrade.
	if err := copyFileBackup(installPath, backupPath); err != nil {
		return "", fmt.Errorf("backup current binary: %w", err)
	}

	// Same directory as installPath so the Rename stays same-device atomic.
	stageF, err := os.CreateTemp(filepath.Dir(installPath), stagingPattern)
	if err != nil {
		_ = os.Remove(backupPath)
		return "", fmt.Errorf("create staging file: %w", err)
	}
	stagePath := stageF.Name()
	// Close first so a copyFile failure does not leak the O_EXCL fd.
	if err := stageF.Close(); err != nil {
		_ = os.Remove(stagePath)
		_ = os.Remove(backupPath)
		return "", fmt.Errorf("close staging file: %w", err)
	}

	if err := copyFile(newBin, stagePath); err != nil {
		_ = os.Remove(stagePath)
		_ = os.Remove(backupPath)
		return "", fmt.Errorf("stage new binary: %w", err)
	}

	if err := os.Rename(stagePath, installPath); err != nil {
		// Join restore errors so a half-broken install dir is visible.
		_ = os.Remove(stagePath)
		errs := []error{fmt.Errorf("rename staged binary into place: %w", err)}
		if rerr := copyFile(backupPath, installPath); rerr != nil {
			errs = append(errs, fmt.Errorf("restore backup after rename failure: %w", rerr))
		} else {
			// The backup is 0600; the restored binary must be executable again.
			if cerr := os.Chmod(installPath, 0o755); cerr != nil {
				errs = append(errs, fmt.Errorf("chmod restored binary: %w", cerr))
			}
			_ = os.Remove(backupPath)
		}
		return "", errors.Join(errs...)
	}

	// Explicit chmod: copyFile copies the source mode, which may still be 0600
	// (or umask-stripped), and an un-executable install fails systemd with 203/EXEC.
	if err := os.Chmod(installPath, 0o755); err != nil {
		// In place but not executable: restore so the service keeps the old binary.
		errs := []error{fmt.Errorf("chmod installed binary 0755: %w", err)}
		if rerr := copyFile(backupPath, installPath); rerr != nil {
			errs = append(errs, fmt.Errorf("restore backup after chmod failure: %w", rerr))
		} else {
			if cerr := os.Chmod(installPath, 0o755); cerr != nil {
				errs = append(errs, fmt.Errorf("chmod restored binary: %w", cerr))
			}
			_ = os.Remove(backupPath)
		}
		return "", errors.Join(errs...)
	}
	return backupPath, nil
}

// Rollback restores backupPath → installPath (re-applying 0755, the backup is
// 0600) and removes the backup. An explicit recovery primitive; the upgrade
// flow never auto-rolls-back on a restart-confirmation timeout.
func Rollback(installPath, backupPath string) error {
	if err := copyFile(backupPath, installPath); err != nil {
		return fmt.Errorf("rollback restore: %w", err)
	}
	if err := os.Chmod(installPath, 0o755); err != nil {
		return fmt.Errorf("rollback chmod: %w", err)
	}
	_ = os.Remove(backupPath)
	return nil
}

// SelfPath returns the absolute path of the running executable.
func SelfPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate running executable: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks on %s: %w", p, err)
	}
	return resolved, nil
}

// ---- internal helpers -------------------------------------------------------

var tagRe = regexp.MustCompile(`/releases/tag/([^/?#]+)$`)

// tagAllowedRe accepts only the semver-ish charset of real tags so a hostile
// redirect cannot smuggle separators or percent-encoding into the asset URL.
var tagAllowedRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func extractTag(rawURL string) string {
	m := tagRe.FindStringSubmatch(rawURL)
	if len(m) < 2 {
		return ""
	}
	tag := m[1]
	if !tagAllowedRe.MatchString(tag) {
		return ""
	}
	return tag
}

// isGitHubHost reports whether host is github.com or a *.github.com subdomain
// (the LatestRelease redirect allowlist).
func isGitHubHost(host string) bool {
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(host)
	if host == "github.com" {
		return true
	}
	return strings.HasSuffix(host, ".github.com")
}

// isGitHubAssetHost is fetchFile's allowlist: assets legitimately 302 to
// objects.githubusercontent.com. Anything else would let a hostile redirect
// replace binary and checksums in lock-step, defeating SHA-256 verification.
func isGitHubAssetHost(host string) bool {
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(host)
	if host == "github.com" || host == "githubusercontent.com" {
		return true
	}
	return strings.HasSuffix(host, ".github.com") ||
		strings.HasSuffix(host, ".githubusercontent.com")
}

// blockPrivateDialContext returns a DialContext that resolves the host and
// rejects reserved IPs (loopback, link-local, private, unspecified), closing
// the DNS rebinding vector where an allowlisted github.com host later resolves
// to IMDS. Returns nil in test mode (testHTTPTransport set) so loopback
// httptest servers work.
func blockPrivateDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	if testHTTPTransport != nil {
		return nil
	}
	dialer := &net.Dialer{}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("selfupdate: malformed dial address %q: %w", addr, err)
		}
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("selfupdate: DNS lookup %q: %w", host, err)
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("selfupdate: DNS lookup %q returned no addresses", host)
		}
		// Any one reserved address rejects the whole dial.
		for _, ia := range addrs {
			ip := ia.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsPrivate() || ip.IsUnspecified() {
				return nil, fmt.Errorf("selfupdate: refused connection to reserved IP %s (DNS rebinding guard)", ip)
			}
		}
		// Dial the validated IP, not the hostname: a second resolution could be
		// answered with a private IP (TOCTOU rebinding). TLS SNI / cert
		// verification still use the URL host via ServerName.
		return dialer.DialContext(ctx, network, net.JoinHostPort(addrs[0].IP.String(), port))
	}
}

// hardenedTransport clones http.DefaultTransport and overrides only
// DialContext, keeping the stdlib timeouts / HTTP/2 defaults a bare
// &http.Transport{} would zero out (#2252).
func hardenedTransport(dialCtx func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = dialCtx
	return t
}

// checkPlatform returns ErrUnsupportedPlatform on operating systems that have
// no entry in the release matrix (currently Windows only).
func checkPlatform() error {
	if runtime.GOOS == "windows" {
		return ErrUnsupportedPlatform
	}
	return nil
}

// assetName returns the release asset filename for the current platform,
// matching what release.yml produces.
func assetName() string {
	return fmt.Sprintf("naozhi-%s-%s", runtime.GOOS, runtime.GOARCH)
}

// testHTTPTransport is set ONLY by selfupdate_test.go to trust
// httptest.NewTLSServer's certificate. Production MUST leave it nil (system
// CA pool); it is the only sanctioned escape from the https-only guards.
var testHTTPTransport http.RoundTripper

func fetchFile(ctx context.Context, fetchURL, dest string, maxBytes int64) error {
	// https-only for the first leg too (CheckRedirect covers later hops); a
	// future http:// caller would otherwise silently lose TLS.
	if !strings.HasPrefix(fetchURL, "https://") {
		return fmt.Errorf("selfupdate: refused non-https URL: %s", fetchURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return fmt.Errorf("build fetch request: %w", err)
	}
	// Parsed-scheme assertion mirrors the redirect guard and survives any
	// relaxation of the prefix gate.
	if req.URL == nil || req.URL.Scheme != "https" {
		return fmt.Errorf("selfupdate: refused non-https URL after parse: %s", fetchURL)
	}
	// Every hop pinned to GitHub hosts — otherwise SHA-256 is not load-bearing,
	// as binary and checksums travel the same path — plus the reserved-IP dial
	// guard (nil in test mode so loopback httptest servers work).
	var transport http.RoundTripper
	if dialCtx := blockPrivateDialContext(); dialCtx != nil {
		transport = hardenedTransport(dialCtx)
	} else {
		transport = testHTTPTransport // nil in production falls back to http.DefaultTransport
	}
	client := &http.Client{
		Timeout:   defaultTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 5 {
				return fmt.Errorf("too many redirects")
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to non-https URL refused: %s", req.URL.Scheme)
			}
			if !isGitHubAssetHost(req.URL.Host) {
				return fmt.Errorf("redirect target outside github.com: %s", req.URL.Host)
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("selfupdate: HTTP request to %s: %w", fetchURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("selfupdate: HTTP %d fetching %s", resp.StatusCode, fetchURL)
	}

	// Owner-only, non-executable until verifyChecksum proves integrity.
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create staging file %s: %w", dest, err)
	}
	defer f.Close()

	// maxBytes+1 surfaces truncation explicitly instead of as a confusing
	// checksum mismatch later.
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("copy response body to %s: %w", dest, err)
	}
	if n > maxBytes {
		return fmt.Errorf("response body exceeds %d bytes (truncated) from %s", maxBytes, fetchURL)
	}
	// Flush to disk before the caller verifies the checksum.
	return f.Sync()
}

func verifyChecksum(binPath, sumPath, asset string) error {
	sums, err := os.ReadFile(sumPath)
	if err != nil {
		return fmt.Errorf("read checksums: %w", err)
	}

	// Each line: "<hash>  <filename>". strings.Fields also swallows a CRLF
	// trailing \r. A duplicate entry for the asset rejects the whole file
	// (#474): first-line-wins leniency would let a tampered file append a
	// second hash.
	expected := ""
	dupSeen := false
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			// Skip comment lines and require exactly 64 hex chars so a "# asset"
			// line cannot become expected="#".
			if strings.HasPrefix(fields[0], "#") {
				continue
			}
			if len(fields[0]) != 64 {
				return fmt.Errorf("checksums.txt: malformed checksum field %q for asset %q (want 64 hex chars)", fields[0], asset)
			}
			if _, err := hex.DecodeString(fields[0]); err != nil {
				return fmt.Errorf("checksums.txt: non-hex checksum field %q for asset %q", fields[0], asset)
			}
			if expected != "" {
				dupSeen = true
				break
			}
			expected = fields[0]
		}
	}
	if dupSeen {
		return fmt.Errorf("checksums.txt: duplicate entry for asset %q — refusing potentially tampered file", asset)
	}
	if expected == "" {
		return fmt.Errorf("no checksum entry for %q in checksums.txt", asset)
	}

	f, err := os.Open(binPath)
	if err != nil {
		return fmt.Errorf("open binary %s: %w", binPath, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash binary %s: %w", binPath, err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expected, actual)
	}
	return nil
}

// copyFile copies src to dst (preserving src mode) and fsyncs the destination.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source %s: %w", src, err)
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat source %s: %w", src, err)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return fmt.Errorf("open destination %s: %w", dst, err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	return out.Sync()
}

// copyFileBackup copies src to dst at 0600 regardless of src's mode, for the
// predictable .bak path on shared install dirs. dst is opened O_EXCL so a
// hostile UID cannot pre-place a symlink and have the write follow it; the
// prior os.Remove unlinks a stale .bak (or such a symlink) without following,
// and O_EXCL still covers the unlink→open gap.
func copyFileBackup(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source %s: %w", src, err)
	}
	defer in.Close()

	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale backup %s: %w", dst, err)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open destination %s: %w", dst, err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	return out.Sync()
}
