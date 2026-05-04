package shim

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteReadStateFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	want := State{
		ShimPID:   99999,
		CLIPID:    88888,
		Socket:    "/tmp/shim-abc.sock",
		AuthToken: "dGVzdHRva2Vu",
		Key:       "weixin:g:roomXYZ",
		SessionID: "sess_roundtrip",
		Workspace: "/home/user/project",
		CLIArgs:   []string{"--skip-permissions", "--output-format", "stream-json"},
		CLIAlive:  true,
		StartedAt: "2026-04-19T10:00:00Z",
	}

	if err := WriteStateFile(path, want); err != nil {
		t.Fatalf("WriteStateFile: %v", err)
	}

	got, err := ReadStateFile(path)
	if err != nil {
		t.Fatalf("ReadStateFile: %v", err)
	}

	if got.Version != stateVersion {
		t.Errorf("Version = %d, want %d", got.Version, stateVersion)
	}
	if got.ShimPID != want.ShimPID {
		t.Errorf("ShimPID = %d, want %d", got.ShimPID, want.ShimPID)
	}
	if got.CLIPID != want.CLIPID {
		t.Errorf("CLIPID = %d, want %d", got.CLIPID, want.CLIPID)
	}
	if got.Socket != want.Socket {
		t.Errorf("Socket = %q, want %q", got.Socket, want.Socket)
	}
	if got.AuthToken != want.AuthToken {
		t.Errorf("AuthToken = %q, want %q", got.AuthToken, want.AuthToken)
	}
	if got.Key != want.Key {
		t.Errorf("Key = %q, want %q", got.Key, want.Key)
	}
	if got.SessionID != want.SessionID {
		t.Errorf("SessionID = %q, want %q", got.SessionID, want.SessionID)
	}
	if got.Workspace != want.Workspace {
		t.Errorf("Workspace = %q, want %q", got.Workspace, want.Workspace)
	}
	if got.CLIAlive != want.CLIAlive {
		t.Errorf("CLIAlive = %v, want %v", got.CLIAlive, want.CLIAlive)
	}
	if len(got.CLIArgs) != len(want.CLIArgs) {
		t.Errorf("CLIArgs len = %d, want %d", len(got.CLIArgs), len(want.CLIArgs))
	} else {
		for i, a := range want.CLIArgs {
			if got.CLIArgs[i] != a {
				t.Errorf("CLIArgs[%d] = %q, want %q", i, got.CLIArgs[i], a)
			}
		}
	}
}

func TestWriteStateFile_CreatesDir(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "sub1", "sub2", "state.json")

	state := State{ShimPID: 1, Socket: "/tmp/x.sock", AuthToken: "dA==", Key: "k"}
	if err := WriteStateFile(nested, state); err != nil {
		t.Fatalf("WriteStateFile failed to create nested dirs: %v", err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Errorf("state file not created: %v", err)
	}
}

func TestWriteStateFile_IsAtomic(t *testing.T) {
	// After WriteStateFile the file should not be a temp file itself.
	dir := t.TempDir()
	path := filepath.Join(dir, "atomic.json")

	if err := WriteStateFile(path, State{ShimPID: 1, Socket: "/tmp/s.sock", AuthToken: "dA==", Key: "k"}); err != nil {
		t.Fatal(err)
	}

	// No temp files should remain
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".shim-state-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected final file at path: %v", err)
	}
}

func TestWriteStateFile_SetsVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v.json")

	// Supply version 0 (zero value), WriteStateFile must overwrite with stateVersion.
	s := State{ShimPID: 1, Socket: "/tmp/s.sock", AuthToken: "dA==", Key: "k"}
	s.Version = 0
	if err := WriteStateFile(path, s); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != stateVersion {
		t.Errorf("Version = %d, want %d", got.Version, stateVersion)
	}
}

func TestReadStateFile_NotFound(t *testing.T) {
	_, err := ReadStateFile("/nonexistent/path/state.json")
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

func TestReadStateFile_BadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("not json {{{"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadStateFile(path)
	if err == nil {
		t.Error("expected error for bad JSON, got nil")
	}
}

func TestReadStateFile_WrongVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wrong_version.json")
	// Write with version 99
	if err := os.WriteFile(path, []byte(`{"version":99,"shim_pid":1,"socket":"/tmp/s.sock","auth_token":"dA==","key":"k"}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadStateFile(path)
	if err == nil {
		t.Error("expected error for wrong version, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported state version") {
		t.Errorf("expected 'unsupported state version' in error, got: %v", err)
	}
}

func TestRemoveStateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remove_me.json")

	// Create a file then remove it
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	RemoveStateFile(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file should have been removed")
	}

	// Calling on nonexistent must not panic
	RemoveStateFile(path)
	RemoveStateFile("/nonexistent/never/existed.json")
}

func TestGenerateToken(t *testing.T) {
	raw1, b64_1, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if len(raw1) != 32 {
		t.Errorf("raw token length = %d, want 32", len(raw1))
	}
	// b64 must decode back to raw
	decoded, err := base64.StdEncoding.DecodeString(b64_1)
	if err != nil {
		t.Fatalf("DecodeString: %v", err)
	}
	if string(decoded) != string(raw1) {
		t.Error("decoded b64 != raw token")
	}

	// Two calls must produce different tokens
	raw2, b64_2, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken second call: %v", err)
	}
	if string(raw1) == string(raw2) {
		t.Error("GenerateToken returned same token twice (collision)")
	}
	if b64_1 == b64_2 {
		t.Error("GenerateToken returned same b64 twice")
	}
}

func TestKeyHash_Properties(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"feishu direct", "feishu:d:alice:general"},
		{"feishu group", "feishu:g:room123"},
		{"weixin", "weixin:g:xyz"},
		{"empty", ""},
		{"unicode", "飞书:d:张三:技术群"},
	}

	seen := map[string]string{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h1 := KeyHash(tc.key)
			h2 := KeyHash(tc.key)

			// Deterministic
			if h1 != h2 {
				t.Errorf("KeyHash(%q) not deterministic: %q vs %q", tc.key, h1, h2)
			}
			// Length = 16 hex chars (8 bytes)
			if len(h1) != 16 {
				t.Errorf("KeyHash(%q) length = %d, want 16", tc.key, len(h1))
			}
			// No collisions with other test keys
			if prev, ok := seen[h1]; ok {
				t.Errorf("collision: KeyHash(%q) == KeyHash(%q) = %q", tc.key, prev, h1)
			}
			seen[h1] = tc.key
		})
	}
}

func TestSocketPath(t *testing.T) {
	keyHash := KeyHash("feishu:d:alice:general")
	path := SocketPath(keyHash)

	if path == "" {
		t.Error("SocketPath returned empty string")
	}
	if !strings.HasSuffix(path, ".sock") {
		t.Errorf("SocketPath = %q, expected .sock suffix", path)
	}
	if !strings.Contains(path, keyHash) {
		t.Errorf("SocketPath = %q, expected to contain key hash %q", path, keyHash)
	}
	// Must contain "naozhi"
	if !strings.Contains(path, "naozhi") {
		t.Errorf("SocketPath = %q, expected to contain 'naozhi'", path)
	}

	// Two calls with same hash must return same path
	path2 := SocketPath(keyHash)
	if path != path2 {
		t.Errorf("SocketPath not deterministic: %q vs %q", path, path2)
	}
}

func TestSocketPath_XDGRuntimeDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	keyHash := KeyHash("xdg:test:key")
	path := SocketPath(keyHash)

	if !strings.HasPrefix(path, dir) {
		t.Errorf("SocketPath = %q, expected prefix %q when XDG_RUNTIME_DIR set", path, dir)
	}
}

func TestStateFilePath(t *testing.T) {
	tests := []struct {
		stateDir   string
		keyHash    string
		wantSuffix string
	}{
		{"/var/lib/naozhi/shims", "abc123", "/var/lib/naozhi/shims/abc123.json"},
		{"/tmp/shims", "feedbeef12345678", "/tmp/shims/feedbeef12345678.json"},
	}
	for _, tc := range tests {
		got := StateFilePath(tc.stateDir, tc.keyHash)
		if got != tc.wantSuffix {
			t.Errorf("StateFilePath(%q, %q) = %q, want %q", tc.stateDir, tc.keyHash, got, tc.wantSuffix)
		}
	}
}
