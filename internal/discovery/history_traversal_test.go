package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadHistory_RejectsTraversalSessionID confirms the IsValidSessionID
// guard at the top of LoadHistory prevents a non-UUID sessionID from being
// joined into a filepath and escaping claudeDir/projects (R20260607-SEC-1).
// A traversal sessionID must return (nil, nil) without touching any file
// outside the projects tree.
func TestLoadHistory_RejectsTraversalSessionID(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, "claude")
	cwd := "/home/ec2-user"
	projectsDir := filepath.Join(claudeDir, "projects", projDirName(cwd))
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Plant a file at the escape target so a successful traversal would
	// produce a non-nil result and fail the assertion below.
	secret := filepath.Join(dir, "secret.jsonl")
	if err := os.WriteFile(secret, []byte(`{"type":"user","message":{"role":"user","content":"leak"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	malicious := []string{
		"../../etc/passwd",
		"../secret",
		"..",
		"not-a-uuid",
		"",
	}
	for _, sid := range malicious {
		entries, err := LoadHistory(claudeDir, sid, cwd)
		if err != nil {
			t.Errorf("LoadHistory(%q) err=%v, want nil", sid, err)
		}
		if entries != nil {
			t.Errorf("LoadHistory(%q) entries=%v, want nil (path-traversal guard breached)", sid, entries)
		}
	}
}
