package shim

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/osutil"
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

	want.Version = stateVersion
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", got, want)
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
		if osutil.IsAtomicTempName(e.Name()) {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected final file at path: %v", err)
	}
}

func TestReadState_SchemaVersionExceedsMaxRejected(t *testing.T) {
	// A state file claiming schema_version > maxSupportedSchemaVersion
	// was written by a newer naozhi with fields/semantics this binary
	// cannot interpret. Reader MUST refuse and return the zero State
	// rather than silently dropping data.
	dir := t.TempDir()
	path := filepath.Join(dir, "future.json")
	payload := `{"version":1,"schema_version":2,"shim_pid":1,"socket":"/tmp/s.sock","auth_token":"dA==","key":"k"}`
	if err := os.WriteFile(path, []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStateFile(path)
	if err == nil {
		t.Fatal("expected error for schema_version > max, got nil")
	}
	if !strings.Contains(err.Error(), "schema_version 2") || !strings.Contains(err.Error(), "max supported 1") {
		t.Errorf("error should name both observed and max schema version, got: %v", err)
	}
	// On schema rejection the returned State must be the zero value; asserting
	// on the fields populated by the rejected payload is enough to prove no
	// partial data leaked through.
	if got.ShimPID != 0 || got.Socket != "" || got.AuthToken != "" || got.Key != "" ||
		got.Version != 0 || got.SchemaVersion != 0 || got.CLIArgs != nil {
		t.Errorf("on schema rejection, returned State must be zero value; got %+v", got)
	}
}

func TestReadState_SchemaVersionEqualToMaxAccepted(t *testing.T) {
	// schema_version == maxSupportedSchemaVersion is inside the
	// supported range and must round-trip without error.
	dir := t.TempDir()
	path := filepath.Join(dir, "max.json")
	want := State{
		SchemaVersion: maxSupportedSchemaVersion,
		ShimPID:       1,
		Socket:        "/tmp/s.sock",
		AuthToken:     "dA==",
		Key:           "k",
	}
	if err := WriteStateFile(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStateFile(path)
	if err != nil {
		t.Fatalf("ReadStateFile: %v", err)
	}
	if got.SchemaVersion != maxSupportedSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, maxSupportedSchemaVersion)
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

	// Calling on nonexistent must not panic (and must NOT attempt a dir
	// fsync, since nothing was removed — #406 symmetry). The missing-parent
	// case exercises the os.Remove-error early return.
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
			// Length = 32 hex chars (16 bytes / 128 bits). Widened from 64
			// bits in #2298 to keep the birthday-collision bound negligible
			// for long-lived hosts that mint billions of distinct keys.
			if len(h1) != 32 {
				t.Errorf("KeyHash(%q) length = %d, want 32", tc.key, len(h1))
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

// TestReadStateFile_RefusesSymlink: the file carries the socket's auth token.
// A 0600 state file somewhere else, linked in at the expected path, passes
// the permission check (Stat follows the link); the read itself must refuse
// it rather than hand that token to Reconnect.
func TestReadStateFile_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := WriteStateFile(target, State{ShimPID: 1, Socket: "/tmp/s.sock", AuthToken: "dA==", Key: "k"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "linked.json")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if got, err := ReadStateFile(path); err == nil {
		t.Fatalf("a symlinked state file was read (token %q), want refused", got.AuthToken)
	}
}

// TestReadStateFile_OversizeRefused: over the cap is refused even when it is
// otherwise a valid, current-version state (padding rides in an unknown
// field, so only the cap can reject it).
func TestReadStateFile_OversizeRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.json")
	body := `{"version":` + strconv.Itoa(stateVersion) + `,"shim_pid":1,"socket":"/tmp/s.sock","auth_token":"dA==","key":"k","pad":"` +
		strings.Repeat("x", maxStateFileBytes) + `"}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStateFile(path); err == nil {
		t.Error("an over-cap state file must be refused")
	}
}

// TestReadStateFile_EmptyIsError: jsonfile reads an empty file as absent; the
// state reader must still report it, because its callers drop the file only
// on an error.
func TestReadStateFile_EmptyIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStateFile(path); err == nil {
		t.Error("an empty state file must be an error")
	}
}
