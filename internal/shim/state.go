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
//   - Version is the hard schema gate; readers refuse a file whose Version !=
//     stateVersion. Kept unchanged for binary compatibility across upgrades.
//   - SchemaVersion is the advisory forward-compat marker: starts at 1,
//     increments only on major-breaking layout changes; additive fields use
//     omitempty without a bump. Readers refuse SchemaVersion >
//     maxSupportedSchemaVersion. Zero on read MUST be interpreted as v1.
type State struct {
	Version int `json:"version"`
	// SchemaVersion: see the struct-level "Versioning contract"; zero = v1.
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
	// SpawnOverlay is the per-request override layer applied on top of the
	// backend defaults when CLIArgs was built (#2494). nil: written by a shim
	// predating the field (reader falls back to legacy comparison); non-nil
	// zero (`{}`): known and empty; populated: overrides in effect. Additive
	// (omitempty, no SchemaVersion bump — a rolled-back binary must still reconnect).
	SpawnOverlay *SpawnOverlay `json:"spawn_overlay,omitempty"`
}

// SpawnOverlay is the per-request spawn layer the session router merges above
// backend defaults (config.yaml cli.model/effort/extra_args, cli.backends[])
// and below per-session dashboard tuning. It records WHAT was requested, not
// the merged result: the drift comparison re-merges it with CURRENT backend
// defaults, so an operator config change is still drift while an agent-level
// override is not (#2494). The shim treats it as opaque metadata.
//
// Extension contract: every argv-bearing per-request field on session.AgentOpts
// must gain a same-named field here, or the drift rebuild misreads it (#2494).
type SpawnOverlay struct {
	Model     string   `json:"model,omitempty"`
	Effort    string   `json:"effort,omitempty"`
	ExtraArgs []string `json:"extra_args,omitempty"`
	// AccessProfile is the RESOLVED profile ID ("" = global baseline); the
	// profile's default_model is looked up from current config at compare time.
	AccessProfile string `json:"access_profile,omitempty"`
	// AppendSystemPrompt is the layered SystemPrompt rendered as
	// --append-system-prompt (#2493); without it every prompted session
	// would read as drift on restart.
	AppendSystemPrompt string `json:"append_system_prompt,omitempty"`
}

// EncodeSpawnOverlay serialises ov for the --spawn-overlay flag. Nil encodes
// to "" (caller omits the flag); a non-nil zero value encodes to "{}" so the
// shim records "known and empty" rather than "unknown" (see State.SpawnOverlay).
func EncodeSpawnOverlay(ov *SpawnOverlay) (string, error) {
	if ov == nil {
		return "", nil
	}
	data, err := json.Marshal(ov)
	if err != nil {
		return "", fmt.Errorf("encode spawn overlay: %w", err)
	}
	return string(data), nil
}

// DecodeSpawnOverlay is the inverse of EncodeSpawnOverlay: "" yields nil
// (flag absent — legacy caller), anything else must be a JSON object.
func DecodeSpawnOverlay(s string) (*SpawnOverlay, error) {
	if s == "" {
		return nil, nil
	}
	var ov SpawnOverlay
	if err := json.Unmarshal([]byte(s), &ov); err != nil {
		return nil, fmt.Errorf("decode spawn overlay: %w", err)
	}
	return &ov, nil
}

const stateVersion = 1

// maxSupportedSchemaVersion is the largest SchemaVersion this build reads;
// higher is refused rather than silently dropping fields.
const maxSupportedSchemaVersion = 1

// WriteStateFile atomically writes the state to path with mode 0600.
//
// AuthToken is stored in plaintext under a 0700 dir + 0600 file: SO_PEERCRED
// already enforces same-UID at the socket, so a same-UID reader could dial
// directly anyway; encrypting at rest would not raise the bar (accepted risk).
// Mkdir + Chmod of the parent dir stay here because osutil.WriteFileAtomic
// does not own the parent mode (#621).
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
		// Don't echo the path: the error can land in HTTP responses via
		// Reconnect; the slog above already has it.
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
		// Warn as well as refuse, so a downgraded binary against newer state
		// leaves a breadcrumb instead of a silent reconnect failure.
		slog.Warn("shim state was written by a newer naozhi; refusing to reconnect",
			"path", path,
			"observed_schema_version", state.SchemaVersion,
			"max_supported_schema_version", maxSupportedSchemaVersion)
		return State{}, fmt.Errorf("shim state schema_version %d > max supported %d (newer naozhi wrote it)", state.SchemaVersion, maxSupportedSchemaVersion)
	}
	return state, nil
}

// RemoveStateFile removes the state file and ignores not-found errors.
//
// The write path fsyncs the parent dir; removal must be symmetric (#406) or
// an unlink of a zombie record could be lost on power loss and restart
// discovery would re-find the dead shim. osutil.SyncDir degrades gracefully.
func RemoveStateFile(path string) {
	if err := os.Remove(path); err != nil {
		return // not-found or other error: nothing durably changed to fsync
	}
	_ = osutil.SyncDir(filepath.Dir(path))
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
	// Re-apply 0700 even when MkdirAll is a no-op: a pre-existing dir may be
	// looser and the socket inherits its traverse-visibility.
	os.Chmod(dir, 0700) //nolint:errcheck
	return filepath.Join(dir, fmt.Sprintf("shim-%s.sock", keyHash))
}

// StateFilePath returns the state file path for a given session key hash.
func StateFilePath(stateDir, keyHash string) string {
	return filepath.Join(stateDir, keyHash+".json")
}

// KeyHash returns a truncated SHA-256 hex hash of the session key (128 bits,
// 32 hex chars). It names both the socket and state files, so a collision
// clobbers a live session's state and reconnect token; 64 bits was
// birthday-bound-exposed on hosts minting billions of keys (#2298).
func KeyHash(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:16])
}
