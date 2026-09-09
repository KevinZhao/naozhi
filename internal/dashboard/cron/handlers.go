package cron

import (
	"sync"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// Cron input bounds shared with the IM `/cron` path. Both surfaces feed the
// same on-disk cron_jobs.json schema, so the limits must stay in lockstep —
// see internal/cron/limits.go.
const (
	maxCronPromptBytesDashboard   = cronpkg.MaxPromptBytes
	maxCronIDLenDashboard         = cronpkg.MaxIDLen
	maxCronScheduleBytesDashboard = cronpkg.MaxScheduleBytes
)

// Handlers groups the cron job management API endpoints.
//
// Injected wiring lives in deps (one Deps value, #2635) rather than a private
// copy per field; the remaining fields are runtime state the handlers own.
type Handlers struct {
	// deps is read-only after New.
	deps Deps

	// missedCache memoises HasMissedSchedule verdicts so the 1 Hz poll does not
	// re-Parse every job's cron expression per poll × tab. Keyed by (jobID,
	// schedule, startedAt) so edits / restarts invalidate by key turnover; a
	// LastRunAt advance invalidates via the lastRunNanos guard (#857).
	missedCacheMu sync.RWMutex
	missedCache   map[string]missedVerdict

	// tzLabelMu guards the memoised timezone label. Keyed on (locName, offset),
	// NOT loc.String() alone: a fixed *time.Location's offset still flips
	// across DST transitions.
	tzLabelMu     sync.RWMutex
	tzLabelLoc    string
	tzLabelOffset int
	tzLabelCached string
	tzLabelHasVal bool

	// transcriptSem is a process-wide cap on concurrent transcript requests
	// (#798): each holds a 256 KB scanner buffer plus an 8 MB read budget, so the
	// per-IP limiter alone lets N operators park N×8 MB. Excess requests get 503.
	transcriptSem chan struct{}
}

// HasRunsLimiter / HasListLimiter / HasWriteLimiter / HasTranscriptLimiter
// expose limiter nil-state for server's boot-time invariant check.
func (h *Handlers) HasRunsLimiter() bool       { return h.deps.RateLimits.Runs != nil }
func (h *Handlers) HasListLimiter() bool       { return h.deps.RateLimits.List != nil }
func (h *Handlers) HasWriteLimiter() bool      { return h.deps.RateLimits.Write != nil }
func (h *Handlers) HasTranscriptLimiter() bool { return h.deps.RateLimits.Transcript != nil }

// RateLimits are the four per-IP budgets the cron endpoints need. They stay
// FOUR limiters, not one: each guards a different cost shape, and sharing a
// bucket would let one endpoint starve another into 429 — the reason
// transcriptLimiter was split out of runsLimiter in the first place. What #2554
// collapses is the Deps SURFACE (4 flat fields → 1 named group), not the
// budgets.
//
// A nil limiter disables that gate, which is how partially-constructed test
// fixtures work; the server's handlerSet.checkLimiters panics at boot if a
// production wiring leaves one nil.
type RateLimits struct {
	// Runs caps /api/cron/runs and /runs/{run_id}.
	Runs IPLimiter
	// List caps GET /api/cron, whose cost grows with the number of jobs.
	List IPLimiter
	// Transcript has its own budget so transcript reads and run reads cannot
	// starve each other.
	Transcript IPLimiter
	// Write caps the cron write/control endpoints (trigger, pause, delete, …).
	Write IPLimiter
}

// Deps bundles all wiring for New.
type Deps struct {
	Scheduler   SchedulerView
	AllowedRoot string
	// ClaudeDir is the absolute path to ~/.claude, used by HandleRunTranscript
	// to locate a run's JSONL. Empty disables the endpoint (fallback:"missing").
	ClaudeDir string
	// RateLimits are the four per-IP budgets; a nil limiter disables that
	// gate (test fixtures). See RateLimits for why they stay four.
	RateLimits RateLimits
	// TranscriptSemCap sizes transcriptSem; 0 leaves the cap off.
	TranscriptSemCap int
	// ValidateWS / ClassifyWSErr inject internal/server's validateWorkspace +
	// classifyWorkspaceErr without reverse-importing server. Both nil-safe.
	ValidateWS    func(ws, root string) (string, error)
	ClassifyWSErr func(err error) (int, string)
}

// New constructs a Handlers from injected deps.
func New(d Deps) *Handlers {
	var sem chan struct{}
	if d.TranscriptSemCap > 0 {
		sem = make(chan struct{}, d.TranscriptSemCap)
	}
	return &Handlers{deps: d, transcriptSem: sem}
}
