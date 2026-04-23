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
type State struct {
	Version         int      `json:"version"`
	ShimPID         int      `json:"shim_pid"`
	CLIPID          int      `json:"cli_pid"`
	Socket          string   `json:"socket"`
	AuthToken       string   `json:"auth_token"`
	Key             string   `json:"key"`
	SessionID       string   `json:"session_id"`
	Workspace       string   `json:"workspace"`
	CLIArgs         []string `json:"cli_args"`
	CLIAlive        bool     `json:"cli_alive"`
	StartedAt       string   `json:"started_at"`
	LastConnectedAt string   `json:"last_connected_at,omitempty"`
	BufferCount     int      `json:"buffer_count"`
}

const stateVersion = 1

// WriteStateFile atomically writes the state to path with mode 0600.
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

	f, err := os.CreateTemp(dir, ".shim-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write temp state: %w", err)
	}
	// Fsync the payload before rename so a crash between data write and
	// rename cannot surface as a zero-byte file that replaces the previous
	// good state. This file is written infrequently (at connect/disconnect
	// lifecycle events) so the fsync cost is negligible.
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("sync temp state: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename state: %w", err)
	}
	// Fsync the parent directory so the rename is durable on XFS and similar
	// filesystems where a rename's directory entry is not automatically
	// persisted. Without this, a power cut right after Rename could leave
	// shim state invisible after reboot — reconnect would fail and live
	// sessions would appear dead until a manual cleanup.
	if err := osutil.SyncDir(dir); err != nil {
		// Soft failure: data is already on disk; the only loss is
		// durability of the directory entry on crash. Logged so ops can
		// correlate if a reboot-time shim loss appears.
		slog.Debug("shim state: fsync dir failed", "dir", dir, "err", err)
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
