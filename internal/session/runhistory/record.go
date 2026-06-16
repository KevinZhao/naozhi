// Package runhistory persists per-run wall-clock timing for ordinary
// (non-cron) sessions and serves the dashboard's run-history timeline and
// summary stats. It mirrors the cron run-history model (one JSON file per
// run + an in-memory recent ring; no index.json) but is deliberately
// leaner: a SessionRun stores no prompt/response bodies, so the cron
// over-cap truncation machinery is unnecessary here.
//
// See docs/rfc/session-run-metrics.md.
package runhistory

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"time"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// Outcome classifies how a single run terminated. It is the dashboard's
// authoritative "did the task complete?" signal, paired with DurationMS.
type Outcome string

const (
	OutcomeCompleted Outcome = "completed" // CLI returned a normal result
	OutcomeError     Outcome = "error"     // transport / process error
	OutcomeTimeout   Outcome = "timeout"   // hit a CLI total/no-output timeout
	OutcomeCanceled  Outcome = "canceled"  // user interrupt / context canceled
)

// SessionRun is one round-trip ("run") through a session: from the moment
// the user message is handed to the CLI until the turn's terminal result.
// Wall-clock is measured by naozhi itself, not self-reported by the CLI.
//
// It intentionally omits prompt and response text: history must not leak
// conversation content cross-tenant, and omitting bodies keeps each record
// tiny (~150 B) so a per-session ring of them stays cheap.
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

// SessionRunStats is the aggregate view shown above a session's timeline.
// Computed on demand from the recent ring; never persisted.
type SessionRunStats struct {
	Count        int   `json:"count"`
	TotalMS      int64 `json:"total_ms"`
	AvgMS        int64 `json:"avg_ms"`
	P50MS        int64 `json:"p50_ms"`
	P95MS        int64 `json:"p95_ms"`
	MaxMS        int64 `json:"max_ms"`
	CompletedCnt int   `json:"completed_count"`
	ErrorCnt     int   `json:"error_count"`
	TimeoutCnt   int   `json:"timeout_count"`
}

// hexIDLen is the byte length of a generated run ID's entropy (8 bytes ->
// 16 hex chars), matching the cron run-ID shape.
const hexIDLen = 8

// NewRunID returns a fresh 16-char lowercase-hex run identifier.
func NewRunID() (string, error) {
	var b [hexIDLen]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// isValidRunID reports whether s is a non-empty lowercase-hex string of at
// most 64 bytes — the gate that keeps stray filenames (temp files, dotfiles,
// path-traversal attempts) out of the on-disk run directory and List output.
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
