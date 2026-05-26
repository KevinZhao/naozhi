package shim

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/naozhi/naozhi/internal/osutil"
)

// State represents the persistent state of a running shim, stored as JSON.
//
// Versioning contract:
//   - Version (legacy "version" tag) is the hard schema version gate; readers
//     refuse to load a file whose Version != stateVersion. Historically the
//     only versioning signal; kept unchanged to preserve binary compatibility
//     across rolling upgrades.
//   - SchemaVersion (canonical forward-compat marker) is the advisory schema
//     version reported on disk. Starts at 1 and increments only on
//     major-breaking layout changes; additive fields use omitempty without a
//     bump. Readers that see SchemaVersion > theirMax SHOULD log a warning
//     and refuse to reconnect — contract documented here, enforcement lands
//     in a follow-up lane. A zero value on read (omitempty default on older
//     writers) MUST be interpreted as v1.
type State struct {
	Version int `json:"version"`
	// SchemaVersion is the advisory forward-compat schema marker. See the
	// struct-level "Versioning contract" godoc above. Omitted when zero;
	// readers treat absent/zero as v1.
	SchemaVersion   int      `json:"schema_version,omitempty"`
	ShimPID         int      `json:"shim_pid"`
	CLIPID          int      `json:"cli_pid"`
	Socket          string   `json:"socket"`
	AuthToken       string   `json:"auth_token"`
	Key             string   `json:"key"`
	SessionID       string   `json:"session_id"`
	Workspace       string   `json:"workspace"`
	Backend         string   `json:"backend,omitempty"` // "claude" | "kiro" | ...
	CLIArgs         []string `json:"cli_args"`
	CLIAlive        bool     `json:"cli_alive"`
	StartedAt       string   `json:"started_at"`
	LastConnectedAt string   `json:"last_connected_at,omitempty"`
	BufferCount     int      `json:"buffer_count"`
}

const stateVersion = 1

// maxSupportedSchemaVersion is the largest SchemaVersion this naozhi
// build knows how to read. A state file claiming a higher version
// may contain fields / semantics this binary doesn't understand,
// so Reader REFUSES to parse it rather than silently dropping data.
// Bump this when the schema grows a new forward-compatible field.
const maxSupportedSchemaVersion = 1

// WriteStateFile atomically writes the state to path with mode 0600.
//
// R215-SEC-P3-1 archive anchor: AuthToken is stored in plaintext under a
// 0700 directory + 0600 file. The shim already enforces same-UID at the
// AF_UNIX layer via SO_PEERCRED (peeruid_linux.go), so a same-UID attacker
// who can read the state file can also dial the socket directly; encrypting
// the token at rest would not raise the bar. Per-user threat model is "OS
// accounts are trust boundaries" — encryption would only obfuscate, not
// secure. Tracked as accepted risk.
//
// R247-ARCH-5 (#621): the write path delegates to osutil.WriteFileAtomic
// rather than reimplementing the temp-write → fsync → rename → fsync-dir
// sequence inline. The previous local copy and the canonical helper had
// drifted on the temp-file naming pattern (".shim-state-*.tmp" here vs
// ".<base>.*.tmp" in osutil); the helper's pattern groups temp files by
// destination basename, which is a strict improvement for crash-recovery
// sweeps. Mkdir + Chmod of the parent state directory remain the caller's
// responsibility (osutil.WriteFileAtomic does not own the parent dir mode
// because callers carry different perm policies — shim wants 0700, other
// stores tolerate 0750).
func WriteStateFile(path string, state State) error {
	state.Version = stateVersion
	data, err := json.MarshalIndent(state, "", "    ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	os.Chmod(dir, 0700) //nolint:errcheck

	if err := osutil.WriteFileAtomic(path, data, 0600); err != nil {
		return fmt.Errorf("write state file %s: %w", path, err)
	}
	return nil
}

// ReadStateFile reads a shim state from the given path.
// Refuses to read if the file is group- or world-accessible — the JSON
// embeds a base64 auth token that grants direct socket attachment, so a
// drifted permission (e.g., a backup tool that re-permed the directory)
// would leak authority. Mirrors the cookie_secret protection pattern.
func ReadStateFile(path string) (State, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return State{}, err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		slog.Warn("shim state file has overly permissive mode — refusing to read",
			"path", path, "mode", fmt.Sprintf("%#o", perm))
		// Do not echo the path in the error string — the error surfaces
		// through Reconnect and can land in HTTP responses; the full path
		// is already captured in the slog above for operator triage.
		return State{}, fmt.Errorf("shim state has insecure permissions %#o", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("parse state %s: %w", path, err)
	}
	if state.Version != stateVersion {
		return State{}, fmt.Errorf("unsupported state version %d (want %d) in %s", state.Version, stateVersion, path)
	}
	if state.SchemaVersion > maxSupportedSchemaVersion {
		return State{}, fmt.Errorf("shim state schema_version %d > max supported %d (newer naozhi wrote it)", state.SchemaVersion, maxSupportedSchemaVersion)
	}
	return state, nil
}

// RemoveStateFile removes the state file and ignores not-found errors.
func RemoveStateFile(path string) {
	_ = os.Remove(path)
}

// GenerateToken creates a cryptographically random token for shim authentication.
func GenerateToken() ([]byte, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", fmt.Errorf("generate token: %w", err)
	}
	return raw, base64.StdEncoding.EncodeToString(raw), nil
}

// SocketPath returns the unix socket path for a given session key hash.
// Prefers XDG_RUNTIME_DIR, falls back to ~/.naozhi/run/.
func SocketPath(keyHash string) string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".naozhi", "run")
	} else {
		dir = filepath.Join(dir, "naozhi")
	}
	os.MkdirAll(dir, 0700) //nolint:errcheck
	// Re-apply 0700 even when MkdirAll is a no-op: a pre-existing directory
	// from an earlier build / backup tool may have looser permissions, and
	// the socket file inherits the directory's traverse-visibility.
	os.Chmod(dir, 0700) //nolint:errcheck
	return filepath.Join(dir, fmt.Sprintf("shim-%s.sock", keyHash))
}

// StateFilePath returns the state file path for a given session key hash.
func StateFilePath(stateDir, keyHash string) string {
	return filepath.Join(stateDir, keyHash+".json")
}

// KeyHash returns a truncated SHA-256 hex hash of the session key.
// Produces a 16-character string with negligible collision probability.
func KeyHash(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:8])
}
