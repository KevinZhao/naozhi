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

	// maxBinaryBytes caps the download size to guard against a rogue release
	// asset or MITM response filling the disk.
	maxBinaryBytes = 200 * 1024 * 1024 // 200 MB

	// maxChecksumBytes caps the size of checksums.txt — a hardened upper
	// bound, far larger than legitimate release manifests (a few KB) but
	// small enough that a hostile mirror cannot exhaust memory by serving a
	// giant file.
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

// LatestRelease queries GitHub for the latest release tag by following the
// /releases/latest redirect. No API token required (anonymous, no rate-limit
// concern for a single query per upgrade call).
func LatestRelease(ctx context.Context) (*Release, error) {
	if err := checkPlatform(); err != nil {
		return nil, fmt.Errorf("check platform: %w", err)
	}

	latestURL := fmt.Sprintf("https://github.com/%s/releases/latest", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// We want the final URL after the redirect, not the body.
	// CheckRedirect pins every hop to github.com / *.github.com so a hostile
	// CDN/DNS cannot send us to evil.com/tag/v9 (extractTag would then accept
	// an attacker-controlled tag string and we'd build the asset URL off it).
	client := &http.Client{
		Timeout: defaultTimeout,
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
	// Defense-in-depth: PathEscape the tag before it joins the download URL
	// so a redirect-leak that smuggled `?x=y` or path separators through the
	// regex extractor cannot pivot to a different host or query.
	base := fmt.Sprintf("https://github.com/%s/releases/download/%s", repo, url.PathEscape(tag))
	return &Release{
		Tag:      tag,
		AssetURL: base + "/" + asset,
		SumURL:   base + "/checksums.txt",
	}, nil
}

// pinSha256EnvVar names the env var an operator can set to pin the expected
// SHA-256 of checksums.txt itself. R238-SEC-4 (#815) defense-in-depth: the
// existing flow verifies the binary's SHA-256 against checksums.txt, but a
// leaked GitHub token lets an attacker swap BOTH files in lock-step, so the
// fetched checksums.txt is no stronger than the leaked-token threat model.
//
// When the operator pre-pins the expected checksums.txt hash (recorded
// out-of-band — e.g. by manually fetching it once on a trusted host and
// noting the SHA-256), we re-verify the live download against that pin
// before parsing. A token-driven swap of both files now also requires the
// attacker to compromise the operator's pinning store, which is a strict
// upgrade over "just exfiltrate the GitHub token".
//
// Empty / unset → behaviour unchanged (best-effort SHA-256 chain to
// checksums.txt). Operators who care can set the var; a fully-signed
// release flow (cosign / Sigstore) is the proper long-term fix and is
// tracked separately under the same issue.
const pinSha256EnvVar = "NAOZHI_UPGRADE_PIN_SHA256"

// pinSha256HexRe matches a 64-char hex SHA-256 digest. Used to reject a
// malformed env var early so the operator sees a clear error instead of a
// silent fallback. Lowercased for the comparison; the regex is
// case-insensitive so operators copy/pasting from a tool that emitted
// uppercase hex still get the pin honoured.
var pinSha256HexRe = regexp.MustCompile(`^[A-Fa-f0-9]{64}$`)

// Download fetches the binary and checksums.txt into dir, verifies the
// SHA-256 checksum, and returns the path to the downloaded binary.
//
// The binary is written 0600 (non-executable, owner-only) until checksum
// verification succeeds, then chmod'd to 0755. This closes a small TOCTOU
// window where a yet-to-be-verified binary could be exec'd by a racing
// process — even though tmp dirs are created 0700 today, defense-in-depth
// means the file mode itself never claims "this is ready to execute"
// before we've confirmed integrity.
//
// R238-SEC-4 (#815): when NAOZHI_UPGRADE_PIN_SHA256 is set, checksums.txt
// must match that pinned hash before we trust its contents. This guards
// against a leaked GitHub token swapping both files in lock-step. The pin
// is a TOFU stopgap pending a proper signing flow (cosign / Sigstore).
func Download(ctx context.Context, rel *Release, dir string) (binPath string, err error) {
	// R20260606-SEC-2 (#1823): if the operator demanded strict integrity
	// (NAOZHI_UPGRADE_REQUIRE_PIN) but no strong-trust anchor exists — no
	// embedded signing key and no out-of-band checksums pin — refuse before
	// touching the network rather than silently trusting the same-channel
	// checksums.txt that a leaked release token could swap in lock-step.
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
	// R238-SEC-4 (#815): verify the operator-pinned hash of checksums.txt
	// before we let verifyChecksum parse it. Pin must come BEFORE the
	// checksum chain because a tampered checksums.txt would otherwise
	// happily verify a tampered binary.
	if err := verifyPinnedChecksumsFile(sumPath); err != nil {
		return "", err
	}
	if err := verifyChecksum(binPath, sumPath, asset); err != nil {
		return "", err
	}
	// Verified: now flip to executable so Replace's copyFile preserves
	// 0755 when staging into the install dir.
	if err := os.Chmod(binPath, 0o755); err != nil {
		return "", fmt.Errorf("chmod verified binary: %w", err)
	}
	return binPath, nil
}

// verifyPinnedChecksumsFile enforces the optional NAOZHI_UPGRADE_PIN_SHA256
// pin on checksums.txt. R238-SEC-4 (#815). Returns nil when the pin is
// unset (current default — ie behaviour unchanged for existing operators).
//
// A non-empty but malformed pin returns an error rather than silently
// downgrading; this is the safer failure mode (operator sees the typo,
// fixes it, retries) versus accidentally bypassing a pin the operator
// believes is active.
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
	// Constant-time compare on lowercased values so operators using
	// uppercase pins still match. The pinSha256HexRe already constrained
	// the input to hex, so ToLower is byte-safe (no UTF-8 surprises).
	expected := strings.ToLower(pin)
	if subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
		return fmt.Errorf("selfupdate: checksums.txt SHA-256 %s does not match pinned %s — refusing upgrade",
			actual, expected)
	}
	return nil
}

// stagingPattern is the os.CreateTemp pattern Replace uses for the
// staging file. Kept as a package-level constant so tests can glob for
// stale staging files after forcing a Replace failure (the random suffix
// makes the exact filename unpredictable). R225-SEC-3.
const stagingPattern = ".naozhi-upgrade-*.staging"

// Replace atomically swaps newBin into installPath:
//  1. Backs up the current binary to installPath + backupSuffix.
//  2. Writes newBin to a staging file in the same directory.
//  3. os.Rename (atomic on same filesystem) stages → installPath.
//  4. On any failure after backup, restores the backup.
//
// The staging file uses os.CreateTemp with a randomised suffix instead of
// a fixed `.naozhi-upgrade.staging` name. On a multi-user host where
// $installDir is writable by more than one UID (shared /usr/local/bin),
// the fixed name gave a hostile UID a known target it could pre-create
// or symlink before the upgrade ran; CreateTemp's O_EXCL+random suffix
// removes the predictability without changing the atomic-rename contract.
// R225-SEC-3.
func Replace(newBin, installPath string) (backupPath string, err error) {
	backupPath = installPath + backupSuffix

	// Backup current binary so we can roll back on service-restart failure.
	// Force 0600 on the backup: the install dir is sometimes shared
	// (/usr/local/bin), and inheriting the live binary's 0755 would leave
	// a world-executable copy of the prior version readable by other UIDs
	// for the duration of the upgrade. Rollback reopens it and chmod 0755
	// after a successful restore.
	if err := copyFileBackup(installPath, backupPath); err != nil {
		return "", fmt.Errorf("backup current binary: %w", err)
	}

	// CreateTemp gives an O_EXCL'd staging file in the same directory as
	// installPath, so the subsequent Rename stays a same-device atomic op.
	stageF, err := os.CreateTemp(filepath.Dir(installPath), stagingPattern)
	if err != nil {
		_ = os.Remove(backupPath)
		return "", fmt.Errorf("create staging file: %w", err)
	}
	stagePath := stageF.Name()
	// Close immediately — copyFile re-opens for write. Closing first means
	// a copyFile failure does not leak the original O_EXCL fd.
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
		// Collect cleanup/restore errors so the caller can see a corrupted
		// install state (e.g. restore failed → install dir half-broken).
		_ = os.Remove(stagePath)
		errs := []error{fmt.Errorf("rename staged binary into place: %w", err)}
		if rerr := copyFile(backupPath, installPath); rerr != nil {
			errs = append(errs, fmt.Errorf("restore backup after rename failure: %w", rerr))
		} else {
			// copyFile preserves the backup's 0600 mode; flip back to
			// 0755 so the restored binary remains executable by systemd.
			if cerr := os.Chmod(installPath, 0o755); cerr != nil {
				errs = append(errs, fmt.Errorf("chmod restored binary: %w", cerr))
			}
			_ = os.Remove(backupPath)
		}
		return "", errors.Join(errs...)
	}

	// Success path: force the installed binary executable. Do NOT rely on
	// copyFile having preserved 0755 — copyFile opens the staging file with
	// the *source* mode, and the downloaded binary can legitimately be 0600
	// (fetchFile writes 0600; Download chmods it 0755 only after checksum
	// verification, and on a loaded host that chmod has been observed not to
	// stick before the stage copy reads st.Mode()). A umask can also strip
	// bits off the OpenFile request. Either way an un-executable install
	// makes systemd fail with 203/EXEC — the v0.0.27 upgrade outage. One
	// explicit chmod here removes all of that dependence.
	if err := os.Chmod(installPath, 0o755); err != nil {
		// The new binary is in place but not executable. Restore the backup
		// so the service keeps running the old (working) binary.
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

// Rollback restores backupPath → installPath and removes the backup file.
// The backup is created with 0600 mode (see copyFileBackup) so we chmod
// the restored binary back to 0755 to keep it executable for systemd.
//
// As of the v0.0.27 fix the upgrade flow no longer auto-rolls-back on a
// restart-confirmation timeout (a slow Type=notify cold start is not a bad
// binary). Rollback is retained as the explicit recovery primitive an
// operator can invoke when an upgrade genuinely needs reverting.
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

// tagAllowedRe accepts only the semver-ish character set that real release
// tags use (e.g. "v1.2.3", "v1.2.3-rc.1"). Refuses path separators, percent-
// encoded bytes, or anything else that could pivot the asset download URL
// to a different path/host when the redirect chain is hostile.
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

// isGitHubHost returns true when host is exactly github.com or any
// *.github.com subdomain. Used by LatestRelease where redirects should stay on
// the github.com proper (HTML release page → /releases/tag/ on github.com).
func isGitHubHost(host string) bool {
	// Strip optional port.
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(host)
	if host == "github.com" {
		return true
	}
	return strings.HasSuffix(host, ".github.com")
}

// isGitHubAssetHost is the looser allowlist used by fetchFile, since release
// asset downloads legitimately 302 from github.com to
// objects.githubusercontent.com. Anything else is refused so a hostile
// redirect can't pivot the binary/checksums fetch to an attacker-controlled
// host (which would defeat SHA-256 verification — both files travel the same
// path and could be replaced in lock-step).
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

// blockPrivateDialContext returns a DialContext function that resolves the
// target hostname and rejects any IP address that belongs to a reserved range
// (loopback, link-local, private, unspecified).  This closes the DNS
// rebinding / SSRF vector where a hostname that passes the github.com
// allowlist check later resolves to an internal address such as
// 169.254.169.254 (AWS/GCP IMDS) or an RFC-1918 address.  (R20260603-SEC-1)
//
// The function is injected into http.Transport.DialContext so every TCP
// connection made by fetchFile goes through the IP guard after DNS
// resolution, before any bytes are exchanged.
//
// The guard is skipped (nil is returned) when testHTTPTransport is set,
// because test servers run on loopback and would be incorrectly rejected.
func blockPrivateDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	if testHTTPTransport != nil {
		// Test mode: do not block loopback so httptest servers work.
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
		// Require every resolved address to be non-reserved (any one private
		// IP rejects the whole dial).
		for _, ia := range addrs {
			ip := ia.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsPrivate() || ip.IsUnspecified() {
				return nil, fmt.Errorf("selfupdate: refused connection to reserved IP %s (DNS rebinding guard)", ip)
			}
		}
		// R20260603-SEC-13: dial the *validated* IP directly rather than the
		// hostname. Passing host back to DialContext would trigger a second DNS
		// resolution that an attacker controlling the authoritative server can
		// answer with a private IP this time (classic TOCTOU DNS rebinding) —
		// the bytes-on-wire connection would then reach the rebound address even
		// though the address we vetted was public. Pinning addrs[0].IP closes
		// that window. TLS SNI / cert verification still uses the URL host via
		// the Transport's TLSClientConfig.ServerName, so name-based TLS is
		// unaffected.
		return dialer.DialContext(ctx, network, net.JoinHostPort(addrs[0].IP.String(), port))
	}
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

// testHTTPTransport is set ONLY by selfupdate_test.go (TestDownload_*)
// to inject a transport that trusts httptest.NewTLSServer's self-signed
// certificate. Production code MUST leave this nil so the default
// transport (system CA pool) is used. R240-SEC-4 (#1048): the entry-point
// https-prefix guard below symmetrically enforces TLS for the first leg
// — the test transport is the only sanctioned escape hatch.
var testHTTPTransport http.RoundTripper

func fetchFile(ctx context.Context, fetchURL, dest string, maxBytes int64) error {
	// R240-SEC-4 (#1048): symmetric https-only guard for the first leg.
	// CheckRedirect already pins subsequent hops to https, but the initial
	// request URL was unchecked; today every caller passes a hardcoded
	// https://github.com URL, but a future caller (or a config-driven
	// override) handing us a http:// URL would silently lose TLS for the
	// first leg and leave verifyChecksum chasing the wrong threat model.
	if !strings.HasPrefix(fetchURL, "https://") {
		return fmt.Errorf("selfupdate: refused non-https URL: %s", fetchURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return fmt.Errorf("build fetch request: %w", err)
	}
	// R247-SEC-5 (#497): belt-and-suspenders check on the parsed URL scheme.
	// The HasPrefix gate above rejects mixed-case schemes ("HTTPS://...")
	// which Go's URL parser would otherwise normalise to scheme=https. That
	// case is harmless — the prefix check is strictly stricter than the
	// scheme check — but a parsed-scheme assertion documents the invariant
	// at the canonical point CheckRedirect uses, mirrors the redirect
	// guard's check shape, and survives any future relaxation of the prefix
	// gate (e.g. a caller that pre-validates scheme separately and trims
	// the `https://` literal before calling). Defense in depth.
	if req.URL == nil || req.URL.Scheme != "https" {
		return fmt.Errorf("selfupdate: refused non-https URL after parse: %s", fetchURL)
	}
	// Pin every hop to github.com / *.github.com so a hostile redirect cannot
	// pivot the binary or checksums.txt download to an attacker-controlled
	// host. Without this guard the SHA-256 verification is no longer
	// load-bearing — both files travel the same path and could be replaced
	// in lock-step. (R228-SEC-1/SEC-9，覆盖 R227-SEC-5 的 https-only 检查)
	//
	// R20260603-SEC-1: inject a DialContext that rejects reserved IPs after
	// DNS resolution to close the DNS rebinding / SSRF path (169.254.169.254,
	// RFC-1918, loopback).  In test mode blockPrivateDialContext() returns nil
	// and testHTTPTransport is used as-is so httptest servers on loopback work.
	var transport http.RoundTripper
	if dialCtx := blockPrivateDialContext(); dialCtx != nil {
		transport = &http.Transport{DialContext: dialCtx}
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
	// Download flips ONLY the binary asset to 0755 after a successful
	// checksum check (checksums.txt is read once and discarded so it
	// stays at 0600). The staging file always lives in a 0700 tempdir
	// so the mode is also covered by the directory ACL.
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create staging file %s: %w", dest, err)
	}
	defer f.Close()

	// Read up to maxBytes+1: a write of (maxBytes+1) bytes means the response
	// body actually exceeded maxBytes and we silently truncated, which would
	// later surface as a confusing "checksum mismatch" instead of the real
	// cause. Surface the truncation explicitly.
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

	// Each line: "<hash>  <filename>".
	// strings.Fields collapses any unicode whitespace, including the
	// trailing \r left behind by CRLF line endings (Windows-built CI
	// runners), so this loop is correct against either LF or CRLF
	// checksums.txt without an extra TrimRight. R235-SEC-7 reviewer
	// misread; documented to deflect the same finding next round.
	//
	// R241-SEC-15 (#474): refuse a checksums.txt that contains MORE than
	// one entry for the same asset. Previously the loop took the first
	// match and ignored the rest, so an attacker who controlled the
	// checksums.txt content could append a second line with a different
	// hash for the same asset and rely on parser leniency. With the
	// release format strictly one-line-per-asset (sha256sum --check
	// itself rejects duplicates), reject the file outright instead so a
	// malformed/tampered checksums.txt fails loudly rather than silently
	// deferring to the first-line wins ordering.
	expected := ""
	dupSeen := false
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			// R20260608133928-SEC-6: validate fields[0] before accepting it as
			// the expected checksum. A comment-style line "# asset" would set
			// expected="#" and produce a misleading "checksum mismatch: expected #"
			// error instead of a clean rejection. Guard: skip comment-prefix lines,
			// and only accept exactly 64 lower-hex characters (SHA-256 output size).
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
	// SHA-256 hex digests are equal-length so length-leak isn't the concern,
	// but constant-time compare is the standard hygiene for any digest
	// equality check on attacker-controllable input.
	if subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expected, actual)
	}
	return nil
}

// copyFile copies src to dst (preserving src mode) and fsyncs the destination.
// Used for backup and rollback where data integrity matters more than speed.
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

// copyFileBackup copies src to dst with owner-only permissions, regardless
// of src's mode bits. Use for the .naozhi-upgrade.bak path on shared
// install directories so the brief window before the backup is removed
// does not expose the prior binary copy as world-readable / executable.
//
// R20260603-SEC-9: dst is opened with O_CREATE|O_EXCL|O_WRONLY so a hostile
// UID on a shared install dir cannot pre-place a symlink at the (predictable)
// .naozhi-upgrade.bak path and have the backup write follow it onto an
// arbitrary file. A normal upgrade leaves a stale .bak from the previous run,
// which would make O_EXCL fail spuriously, so we first os.Remove(dst) (a
// regular unlink that also breaks any pre-placed symlink without following
// it). O_EXCL then still guards the TOCTOU window between the unlink and the
// open: an attacker who re-creates a symlink in that gap is rejected rather
// than followed.
func copyFileBackup(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source %s: %w", src, err)
	}
	defer in.Close()

	// Clear any leftover backup (or pre-placed symlink) from a prior run.
	// os.Remove unlinks the path itself; it never follows a symlink to its
	// target, so a malicious link is severed rather than written through.
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
