// Package selfupdate fetches a newer naozhi binary from GitHub Releases,
// verifies its SHA-256 checksum, atomically replaces the running binary,
// and optionally restarts the system service.
//
// Flow:
//
//	LatestVersion()     → GitHub redirect → semver tag
//	Download()          → binary + checksums.txt → tmp dir
//	VerifyChecksum()    → sha256 match
//	Replace()           → backup current, os.Rename new binary
//	RestartService()    → systemctl restart / launchctl reload
package selfupdate

import (
	"context"
	"crypto/sha256"
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
)

// ErrAlreadyLatest is returned by CheckAndDownload when the running version
// matches the latest release.
var ErrAlreadyLatest = errors.New("already at the latest version")

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

// Download fetches the binary and checksums.txt into dir, returning the path
// to the downloaded binary.
func Download(ctx context.Context, rel *Release, dir string) (binPath string, err error) {
	asset := assetName()
	binPath = filepath.Join(dir, asset)

	if err := fetchFile(ctx, rel.AssetURL, binPath); err != nil {
		return "", fmt.Errorf("download binary: %w", err)
	}
	sumPath := filepath.Join(dir, "checksums.txt")
	if err := fetchFile(ctx, rel.SumURL, sumPath); err != nil {
		return "", fmt.Errorf("download checksums: %w", err)
	}
	if err := verifyChecksum(binPath, sumPath, asset); err != nil {
		return "", err
	}
	return binPath, nil
}

// Replace atomically swaps newBin into installPath:
//  1. Backs up the current binary to installPath + backupSuffix.
//  2. Copies newBin over installPath (os.Rename may fail across devices).
//  3. On any failure after backup, restores the backup.
func Replace(newBin, installPath string) (backupPath string, err error) {
	backupPath = installPath + backupSuffix

	// Backup current binary.
	if err := copyFile(installPath, backupPath); err != nil {
		return "", fmt.Errorf("backup current binary: %w", err)
	}

	if err := copyFile(newBin, installPath); err != nil {
		// Restore backup on failure.
		_ = copyFile(backupPath, installPath)
		_ = os.Remove(backupPath)
		return "", fmt.Errorf("replace binary: %w", err)
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

// assetName returns the release asset filename for the current platform,
// matching what release.yml produces.
func assetName() string {
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	return fmt.Sprintf("naozhi-%s-%s", goos, goarch)
}

func fetchFile(ctx context.Context, url, dest string) error {
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

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
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
	if actual != expected {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expected, actual)
	}
	return nil
}

// copyFile copies src to dst, preserving the src file mode.
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

	_, err = io.Copy(out, in)
	return err
}
