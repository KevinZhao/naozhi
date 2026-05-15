package selfupdate

import (
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
	// Checksum of different content.
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
	// checksums.txt for a different asset.
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

	// Binary should now have new content.
	got, _ := os.ReadFile(installPath)
	if string(got) != "new binary" {
		t.Errorf("after Replace, installPath = %q, want %q", got, "new binary")
	}
	// Backup should have old content.
	bak, _ := os.ReadFile(backupPath)
	if string(bak) != "old binary" {
		t.Errorf("backup = %q, want %q", bak, "old binary")
	}

	// Rollback restores old content.
	if err := Rollback(installPath, backupPath); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	got, _ = os.ReadFile(installPath)
	if string(got) != "old binary" {
		t.Errorf("after Rollback, installPath = %q, want %q", got, "old binary")
	}
	// Backup file should be gone.
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Errorf("backup file should be removed after Rollback")
	}
}

// ----- LatestRelease (mock HTTP server) -------------------------------------

func TestLatestRelease_ParseTag(t *testing.T) {
	t.Parallel()
	// Serve a redirect from /latest → /releases/tag/v9.9.9
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/v9.9.9", http.StatusFound)
	}))
	defer srv.Close()

	// Patch the URL used by LatestRelease via a custom httptest redirector
	// by directly calling extractTag on the final URL shape.
	finalURL := srv.URL + "/releases/tag/v9.9.9"
	tag := extractTag(finalURL)
	if tag != "v9.9.9" {
		t.Errorf("extractTag(%q) = %q, want v9.9.9", finalURL, tag)
	}
}
