package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// DiscoveredSession represents a Claude CLI process found on the system.
type DiscoveredSession struct {
	PID       int    `json:"pid"`
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	StartedAt int64  `json:"started_at"` // unix ms
	Kind      string `json:"kind"`       // "interactive" etc.
	Entrypoint string `json:"entrypoint"` // "cli" etc.
}

// sessionFile mirrors the JSON schema of ~/.claude/sessions/{PID}.json.
type sessionFile struct {
	PID        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	CWD        string `json:"cwd"`
	StartedAt  int64  `json:"startedAt"`
	Kind       string `json:"kind"`
	Entrypoint string `json:"entrypoint"`
}

// Scan reads ~/.claude/sessions/*.json and returns live Claude CLI processes
// that are not managed by naozhi (excluded via excludePIDs).
func Scan(claudeDir string, excludePIDs map[int]bool) ([]DiscoveredSession, error) {
	sessDir := filepath.Join(claudeDir, "sessions")
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var result []DiscoveredSession
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(sessDir, entry.Name()))
		if err != nil {
			continue
		}

		var sf sessionFile
		if err := json.Unmarshal(data, &sf); err != nil {
			continue
		}

		if sf.PID <= 0 || sf.SessionID == "" {
			continue
		}

		// Skip naozhi-managed processes
		if excludePIDs[sf.PID] {
			continue
		}

		// Check if process is alive
		if !processAlive(sf.PID) {
			continue
		}

		result = append(result, DiscoveredSession{
			PID:        sf.PID,
			SessionID:  sf.SessionID,
			CWD:        sf.CWD,
			StartedAt:  sf.StartedAt,
			Kind:       sf.Kind,
			Entrypoint: sf.Entrypoint,
		})
	}
	return result, nil
}

// processAlive checks whether a process with the given PID exists.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 tests existence without actually sending a signal.
	return proc.Signal(syscall.Signal(0)) == nil
}
