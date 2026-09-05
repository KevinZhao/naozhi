package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFingerprintConfig(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoad_Fingerprint pins the #2538 contract: the fingerprint hashes the
// file's RAW bytes — a comment-only edit changes it, an mtime-only touch
// does not — and carries the load time and path.
func TestLoad_Fingerprint(t *testing.T) {
	dir := t.TempDir()
	const base = "platforms:\n  weixin:\n    token: \"t\"\n"
	path := writeFingerprintConfig(t, dir, base)

	cfg1, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg1.Fingerprint.SHA256) != 64 {
		t.Fatalf("SHA256 = %q, want 64 hex chars", cfg1.Fingerprint.SHA256)
	}
	if cfg1.Fingerprint.Path != path {
		t.Errorf("Path = %q, want %q", cfg1.Fingerprint.Path, path)
	}
	if time.Since(cfg1.Fingerprint.LoadedAt) > time.Minute {
		t.Errorf("LoadedAt = %v, want ~now", cfg1.Fingerprint.LoadedAt)
	}

	// Same bytes, new mtime: hash identical.
	if err := os.Chtimes(path, time.Now(), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg2.Fingerprint.SHA256 != cfg1.Fingerprint.SHA256 {
		t.Errorf("mtime-only change altered the hash: %s vs %s", cfg2.Fingerprint.SHA256, cfg1.Fingerprint.SHA256)
	}

	// A comment-only edit (parse-identical) must change the hash: the
	// fingerprint is over raw bytes, not the parsed structure.
	writeFingerprintConfig(t, dir, base+"# tuned\n")
	cfg3, err := Load(path)
	if err != nil {
		t.Fatalf("reload after edit: %v", err)
	}
	if cfg3.Fingerprint.SHA256 == cfg1.Fingerprint.SHA256 {
		t.Error("comment-only edit did not change the hash; fingerprint must cover raw bytes")
	}
}
