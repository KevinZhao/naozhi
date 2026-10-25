package cron

import (
	"sync"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// Cron input bounds, from internal/cron/limits.go.
const (
	maxCronIDLenDashboard         = cronpkg.MaxIDLen
	maxCronScheduleBytesDashboard = cronpkg.MaxScheduleBytes
)

// sandboxWorkDirMsg answers cronpkg.ErrSandboxWorkDir on create and update.
const sandboxWorkDirMsg = "云沙箱暂不支持工作目录（Phase 1）：请先清空 work_dir 或改用本机运行"

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
	// (#798): each holds a 256 KB scanner buffer plus an 8 MB read budget, so
	// without it N concurrent readers park N×8 MB. Excess requests get 503.
	transcriptSem chan struct{}
}

// Deps bundles all wiring for New.
type Deps struct {
	Scheduler   SchedulerView
	AllowedRoot string
	// ClaudeDir is the absolute path to ~/.claude, used by HandleRunTranscript
	// to locate a run's JSONL. Empty disables the endpoint (fallback:"missing").
	ClaudeDir string
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
