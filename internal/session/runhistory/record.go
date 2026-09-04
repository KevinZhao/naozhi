// Package runhistory persists per-run wall-clock timing for ordinary
// (non-cron) sessions and serves the dashboard's run-history timeline and
// stats: one JSON file per run + an in-memory recent ring, no index.json.
// See docs/rfc/session-run-metrics.md.
package runhistory

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"time"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// Outcome classifies how a single run terminated.
type Outcome string

const (
	OutcomeCompleted Outcome = "completed" // CLI returned a normal result
	OutcomeError     Outcome = "error"     // transport / process error
	OutcomeTimeout   Outcome = "timeout"   // hit a CLI total/no-output timeout
	OutcomeCanceled  Outcome = "canceled"  // user interrupt / context canceled
)

// SessionRun is one round-trip through a session, from handing the user
// message to the CLI until the turn's terminal result; wall-clock measured
// by naozhi itself. Prompt and response text are omitted so history cannot
// leak conversation content cross-tenant and each record stays ~150 B.
type SessionRun struct {
	RunID       string                  `json:"run_id"`               // 16-char hex, naozhi-generated
	SessionKey  string                  `json:"session_key"`          // {channel}:{chatType}:{id}
	SessionID   string                  `json:"session_id,omitempty"` // CLI session ID (runtime)
	StartedAt   time.Time               `json:"started_at"`
	EndedAt     time.Time               `json:"ended_at"`
	DurationMS  int64                   `json:"duration_ms"`             // wall-clock, >= 0
	FirstByteMS int64                   `json:"first_byte_ms,omitempty"` // StartedAt -> first CLI event
	Outcome     Outcome                 `json:"outcome"`
	ErrorClass  runtelemetry.ErrorClass `json:"error_class,omitempty"`
	CostUSD     float64                 `json:"cost_usd,omitempty"`
}

// SessionRunStats is the aggregate view above a session's timeline; never persisted.
type SessionRunStats struct {
	Count        int     `json:"count"`
	TotalMS      int64   `json:"total_ms"`
	AvgMS        int64   `json:"avg_ms"`
	P50MS        int64   `json:"p50_ms"`
	MaxMS        int64   `json:"max_ms"`
	TotalCostUSD float64 `json:"total_cost_usd,omitempty"`
	CompletedCnt int     `json:"completed_count"`
	ErrorCnt     int     `json:"error_count"`
	TimeoutCnt   int     `json:"timeout_count"`
}

// hexIDLen is the run-ID entropy in bytes (8 -> 16 hex chars), like cron.
const hexIDLen = 8

// NewRunID returns a fresh 16-char lowercase-hex run identifier.
func NewRunID() (string, error) {
	var b [hexIDLen]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// isValidRunID reports whether s is non-empty lowercase hex of at most 64
// bytes — the gate keeping stray filenames / path traversal out of the run
// directory and List output.
func isValidRunID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
