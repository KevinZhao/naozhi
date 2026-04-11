package shim

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		os.Remove(tmp)
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

// ReadStateFile reads a shim state from the given path.
func ReadStateFile(path string) (State, error) {
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
	os.Remove(path)
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
