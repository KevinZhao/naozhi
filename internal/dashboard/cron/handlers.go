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
type Handlers struct {
	scheduler   SchedulerView
	allowedRoot string
	// claudeDir is the absolute path to ~/.claude, used by HandleRunTranscript
	// to locate a run's JSONL. Empty disables the endpoint (fallback:"missing").
	claudeDir string
	// runsLimiter caps per-IP rate of /api/cron/runs and /runs/{run_id}; both
	// fan out filesystem I/O, so a stolen token could otherwise enumerate the
	// run history at unbounded rate. Nil disables the gate (test fixtures).
	runsLimiter IPLimiter

	// listLimiter caps GET /api/cron (1 Hz poll; cost grows with N jobs ×
	// RecentRuns(5)). 2 req/s with burst 30 absorbs a tab refresh storm while
	// capping a parallel-poll attacker per source IP. Nil disables the gate.
	listLimiter IPLimiter

	// transcriptLimiter gives the transcript endpoint its own per-IP budget
	// (#1096): it is far more expensive than runs list/detail, so sharing
	// runsLimiter let either side starve the other into 429. Nil disables the gate.
	transcriptLimiter IPLimiter

	// writeLimiter caps per-IP rate of cron write/control endpoints: trigger
	// spawns the job's claude CLI subprocess and may send IM notifications
	// (loop-trigger amplification); preview runs the parser up to 10 times.
	// 30 req/min with burst 6. Nil disables the gate.
	writeLimiter IPLimiter

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

	// validateWS / classifyWSErr inject internal/server's validateWorkspace +
	// classifyWorkspaceErr without reverse-importing server. Both nil-safe.
	validateWS    func(ws, root string) (string, error)
	classifyWSErr func(err error) (int, string)
}

// HasRunsLimiter / HasListLimiter / HasWriteLimiter / HasTranscriptLimiter
// expose limiter nil-state for server's boot-time invariant check.
func (h *Handlers) HasRunsLimiter() bool       { return h.runsLimiter != nil }
func (h *Handlers) HasListLimiter() bool       { return h.listLimiter != nil }
func (h *Handlers) HasWriteLimiter() bool      { return h.writeLimiter != nil }
func (h *Handlers) HasTranscriptLimiter() bool { return h.transcriptLimiter != nil }

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
	Scheduler        SchedulerView
	AllowedRoot      string
	ClaudeDir        string
	RateLimits       RateLimits
	TranscriptSemCap int
	ValidateWS       func(ws, root string) (string, error)
	ClassifyWSErr    func(err error) (int, string)
}

// New constructs a Handlers from injected deps.
func New(d Deps) *Handlers {
	var sem chan struct{}
	if d.TranscriptSemCap > 0 {
		sem = make(chan struct{}, d.TranscriptSemCap)
	}
	return &Handlers{
		scheduler:         d.Scheduler,
		allowedRoot:       d.AllowedRoot,
		claudeDir:         d.ClaudeDir,
		runsLimiter:       d.RateLimits.Runs,
		listLimiter:       d.RateLimits.List,
		transcriptLimiter: d.RateLimits.Transcript,
		writeLimiter:      d.RateLimits.Write,
		transcriptSem:     sem,
		validateWS:        d.ValidateWS,
		classifyWSErr:     d.ClassifyWSErr,
	}
}
