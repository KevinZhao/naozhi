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
	"net/http"
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
		return nil, err
	}

	url := fmt.Sprintf("https://github.com/%s/releases/latest", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// We want the final URL after the redirect, not the body.
	client := &http.Client{
		Timeout: defaultTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 5 {
				return fmt.Errorf("too many redirects")
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
	base := fmt.Sprintf("https://github.com/%s/releases/download/%s", repo, tag)
	return &Release{
		Tag:      tag,
		AssetURL: base + "/" + asset,
		SumURL:   base + "/checksums.txt",
	}, nil
}

// Download fetches the binary and checksums.txt into dir, verifies the
// SHA-256 checksum, and returns the path to the downloaded binary.
//
// The binary is written 0600 (non-executable, owner-only) until checksum
// verification succeeds, then chmod'd to 0755. This closes a small TOCTOU
// window where a yet-to-be-verified binary could be exec'd by a racing
// process — even though tmp dirs are created 0700 today, defense-in-depth
// means the file mode itself never claims "this is ready to execute"
// before we've confirmed integrity.
func Download(ctx context.Context, rel *Release, dir string) (binPath string, err error) {
	asset := assetName()
	binPath = filepath.Join(dir, asset)

	if err := fetchFile(ctx, rel.AssetURL, binPath, maxBinaryBytes); err != nil {
		return "", fmt.Errorf("download binary: %w", err)
	}
	sumPath := filepath.Join(dir, "checksums.txt")
	if err := fetchFile(ctx, rel.SumURL, sumPath, maxChecksumBytes); err != nil {
		return "", fmt.Errorf("download checksums: %w", err)
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

// Replace atomically swaps newBin into installPath:
//  1. Backs up the current binary to installPath + backupSuffix.
//  2. Writes newBin to a staging file in the same directory.
//  3. os.Rename (atomic on same filesystem) stages → installPath.
//  4. On any failure after backup, restores the backup.
func Replace(newBin, installPath string) (backupPath string, err error) {
	backupPath = installPath + backupSuffix
	stagePath := installPath + ".naozhi-upgrade.staging"

	// Backup current binary so we can roll back on service-restart failure.
	if err := copyFile(installPath, backupPath); err != nil {
		return "", fmt.Errorf("backup current binary: %w", err)
	}

	// Write to a staging file in the same directory (guarantees same device
	// for the subsequent Rename, which is atomic on POSIX).
	if err := copyFile(newBin, stagePath); err != nil {
		_ = os.Remove(backupPath)
		return "", fmt.Errorf("stage new binary: %w", err)
	}

	if err := os.Rename(stagePath, installPath); err != nil {
		_ = os.Remove(stagePath)
		_ = copyFile(backupPath, installPath) // best-effort restore
		_ = os.Remove(backupPath)
		return "", fmt.Errorf("rename staged binary into place: %w", err)
	}
	return backupPath, nil
}

// Rollback restores backupPath → installPath and removes the backup file.
func Rollback(installPath, backupPath string) error {
	if err := copyFile(backupPath, installPath); err != nil {
		return fmt.Errorf("rollback restore: %w", err)
	}
	_ = os.Remove(backupPath)
	return nil
}

// SelfPath returns the absolute path of the running executable.
func SelfPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// ---- internal helpers -------------------------------------------------------

var tagRe = regexp.MustCompile(`/releases/tag/([^/?#]+)$`)

func extractTag(url string) string {
	m := tagRe.FindStringSubmatch(url)
	if len(m) < 2 {
		return ""
	}
	return m[1]
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

func fetchFile(ctx context.Context, url, dest string, maxBytes int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: defaultTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, url)
	}

	// Owner-only, non-executable until verifyChecksum proves integrity.
	// Download flips ONLY the binary asset to 0755 after a successful
	// checksum check (checksums.txt is read once and discarded so it
	// stays at 0600). The staging file always lives in a 0700 tempdir
	// so the mode is also covered by the directory ACL.
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(f, io.LimitReader(resp.Body, maxBytes)); err != nil {
		return err
	}
	// Flush to disk before the caller verifies the checksum.
	return f.Sync()
}

func verifyChecksum(binPath, sumPath, asset string) error {
	sums, err := os.ReadFile(sumPath)
	if err != nil {
		return fmt.Errorf("read checksums: %w", err)
	}

	// Each line: "<hash>  <filename>"
	expected := ""
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			expected = fields[0]
			break
		}
	}
	if expected == "" {
		return fmt.Errorf("no checksum entry for %q in checksums.txt", asset)
	}

	f, err := os.Open(binPath)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
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
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
