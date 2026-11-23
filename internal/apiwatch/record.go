// Package apiwatch keeps what the CLI backends said about their model API
// calls — each retry, each turn's timing and failure, each turn the watchdog
// cut off — so the dashboard can show when and why calls to the model went
// wrong (the 系统 view's 接口监控 section, GET /api/system/apiwatch).
//
// The data comes from the claude CLI's own stream-json frames: a
// system/api_retry frame per retried request (attempt, HTTP status, error
// class, first-byte wait) and the result frame's duration_api_ms / ttft_ms /
// api_error_status. The CLI does all the retrying; this package only records.
package apiwatch

import (
	"strings"
	"time"
)

// Kind is what a Record reports.
type Kind string

const (
	// KindRetry is one request the CLI retried (system/api_retry).
	KindRetry Kind = "retry"
	// KindTurn is one finished turn: its timing and whether it failed.
	KindTurn Kind = "turn"
	// KindTimeout is a turn naozhi's watchdog killed.
	KindTimeout Kind = "timeout"
)

// Record is one observation. Which fields are set depends on Kind.
type Record struct {
	At      time.Time `json:"at"`
	Kind    Kind      `json:"kind"`
	Key     string    `json:"key"`
	Backend string    `json:"backend,omitempty"`
	Model   string    `json:"model,omitempty"`

	// Status is the HTTP status of the failed request; 0 when no response
	// came (a connection error, a timeout).
	Status int `json:"status,omitempty"`
	// Category is the CLI's class of the error (rate_limit, overloaded,
	// authentication_failed, cloud_credential_error, server_error, unknown, …).
	Category string `json:"category,omitempty"`

	// Retry: the attempt that failed, of MaxRetries; the wait before the
	// next; WaitedMS when the API sent no response headers within the CLI's
	// first-byte window.
	Attempt      int   `json:"attempt,omitempty"`
	MaxRetries   int   `json:"max_retries,omitempty"`
	RetryDelayMS int64 `json:"retry_delay_ms,omitempty"`
	WaitedMS     int64 `json:"waited_ms,omitempty"`

	// Turn: wall time, time spent in API calls, time to the first token;
	// Failed with Error (the CLI's error text, capped) when the turn ended in
	// an error.
	DurationMS int64  `json:"duration_ms,omitempty"`
	APIMS      int64  `json:"api_ms,omitempty"`
	TTFTMS     int64  `json:"ttft_ms,omitempty"`
	Failed     bool   `json:"failed,omitempty"`
	Error      string `json:"error,omitempty"`

	// Timeout: the watchdog's reason (no_output_timeout, total_timeout).
	Reason string `json:"reason,omitempty"`

	// UUID is the frame's id, used to drop a frame seen twice.
	UUID string `json:"uuid,omitempty"`
}

// maxErrorRunes caps Record.Error: enough for the API's message, not a
// transcript.
const maxErrorRunes = 300

// Class is the dashboard's grouping of a problem record.
type Class string

const (
	ClassNone       Class = ""
	ClassRegion     Class = "region"     // 400: the caller's region is refused
	ClassAuth       Class = "auth"       // 401/403, or credentials could not be resolved
	ClassRateLimit  Class = "rate_limit" // 429
	ClassOverloaded Class = "overloaded" // 529
	ClassServer     Class = "server"     // other 5xx
	ClassClient     Class = "client"     // other 4xx
	ClassFirstByte  Class = "first_byte" // no response headers within the first-byte window
	ClassTimeout    Class = "timeout"    // the request timed out
	ClassNetwork    Class = "network"    // no response at all
	ClassWatchdog   Class = "watchdog"   // naozhi's watchdog killed the turn
	ClassOther      Class = "other"      // a failed turn with nothing to tell it by
)

// Classes lists every class, in the order the dashboard shows them.
func Classes() []Class {
	return []Class{ClassRegion, ClassAuth, ClassRateLimit, ClassOverloaded, ClassServer,
		ClassClient, ClassFirstByte, ClassTimeout, ClassNetwork, ClassWatchdog, ClassOther}
}

// Problem reports whether r is a problem: a retry, a failed turn or a
// watchdog timeout. A turn that went through is not one, however slow.
func (r Record) Problem() bool {
	return r.Kind == KindRetry || r.Kind == KindTimeout || (r.Kind == KindTurn && r.Failed)
}

// Classify groups a problem record; ClassNone for a record that is not one.
func (r Record) Classify() Class {
	if !r.Problem() {
		return ClassNone
	}
	if r.Kind == KindTimeout {
		return ClassWatchdog
	}
	text := strings.ToLower(r.Error)
	switch {
	case r.WaitedMS > 0:
		return ClassFirstByte
	case strings.Contains(text, "unsupported countries") || strings.Contains(text, "unsupported region"):
		return ClassRegion
	case r.Category == "cloud_credential_error" || r.Category == "authentication_failed" ||
		r.Status == 401 || r.Status == 403 ||
		strings.Contains(text, "credential") || strings.Contains(text, "security token"):
		return ClassAuth
	case r.Status == 429 || r.Category == "rate_limit":
		return ClassRateLimit
	case r.Status == 529 || r.Category == "overloaded":
		return ClassOverloaded
	case r.Status >= 500:
		return ClassServer
	case r.Status >= 400:
		return ClassClient
	case strings.Contains(text, "timed out") || strings.Contains(text, "timeout"):
		return ClassTimeout
	case r.Kind == KindRetry || strings.Contains(text, "connection"):
		return ClassNetwork
	}
	return ClassOther
}

// clip trims s to maxErrorRunes runes.
func clip(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxErrorRunes {
		return string(r[:maxErrorRunes]) + "…"
	}
	return s
}
