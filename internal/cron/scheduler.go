package cron

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	robfigcron "github.com/robfig/cron/v3"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/textutil"
)

// ErrJobNotFound is returned by lookup/mutation APIs when no cron job matches.
// Callers should use errors.Is(err, cron.ErrJobNotFound) instead of string matching.
var ErrJobNotFound = errors.New("cron: job not found")

// ErrJobAlreadyPaused is returned by PauseJob when the target job is already
// paused. Callers (especially HTTP handlers) should map this to 409 Conflict
// rather than 400, since the request was well-formed but the target state is
// incompatible.
var ErrJobAlreadyPaused = errors.New("cron: job already paused")

// ErrJobNotPaused is returned by ResumeJob when the target job is not paused.
var ErrJobNotPaused = errors.New("cron: job not paused")

// ErrJobPaused is returned by TriggerNow when the target job is paused, so a
// manual trigger from the dashboard is rejected instead of silently running
// against the operator's pause intent.
var ErrJobPaused = errors.New("cron: job is paused")

// ErrJobNoPrompt is returned by TriggerNow when the target job has no prompt
// configured. Sentinel form so dashboard handlers can errors.Is and emit an
// HTTP 422 (vs 400) instead of relying on string matching.
var ErrJobNoPrompt = errors.New("cron: job has no prompt")

// ErrPersistFailed is returned by mutation APIs (AddJob/DeleteJob/Update/
// Pause/Resume/SetJobPrompt) when the post-mutation JSON serialisation fails.
// The in-memory state has already been changed and cannot be rolled back —
// marshal failure is observationally unrecoverable (OOM / type-system bug),
// so the caller MUST surface this as a 500-class error so the operator can
// intervene (restart the process or rebuild the job). R51-QUAL-001: prior
// to this sentinel, persistJobsLocked returned a silent no-op func on
// marshal error, causing DeleteJob to "succeed" via the API while the
// deletion never reached disk — a restart replayed the deleted job.
var ErrPersistFailed = errors.New("cron: persist jobs failed")

// SessionRouter is the subset of session.Router that the cron Scheduler
// actually consumes. Declaring it here (consumer-side interface, Go idiom)
// inverts the historical cron → session dependency: a future Router refactor
// only has to preserve these three method shapes to stay scheduler-
// compatible, and tests can inject a fake without pulling the whole router
// graph. Any new s.router.X() call requires extending this interface, which
// makes accidental surface-area growth a compile error instead of a silent
// regression.
type SessionRouter interface {
	// RegisterCronStub creates (or refreshes) a suspended exempt session
	// entry so the cron job shows up in the dashboard sidebar before its
	// first run. Key is always "cron:<jobID>".
	RegisterCronStub(key, workspace, lastPrompt string)
	// RegisterCronStubWithChain 在 RegisterCronStub 的基础上额外注入
	// 一个 session-ID 链，赋给 stub 的 prevSessionIDs。这样 fresh_context
	// cron 每次 Reset 后新建的 stub 仍然能通过 historySource 查到上一次
	// 成功运行留下的 JSONL 历史（~/.claude/projects/<cwd>/<id>.jsonl）。
	// chainIDs 为空 / nil 时等同于 RegisterCronStub。
	RegisterCronStubWithChain(key, workspace, lastPrompt string, chainIDs []string)
	// Reset discards the session for the given key (used by fresh-mode
	// cron jobs and by Delete/Rename flows).
	Reset(key string)
	// GetOrCreate returns an existing session or spawns a new one at
	// execute time. The SessionStatus and *ManagedSession escape the
	// cron package because the scheduler needs to call Send on them.
	GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
}

// SchedulerConfig holds configuration for the cron scheduler.
type SchedulerConfig struct {
	// Router is the session router the scheduler talks to. Accepts the
	// SessionRouter interface so tests can pass a minimal fake; production
	// passes a *session.Router which satisfies it transparently.
	Router        SessionRouter
	Platforms     map[string]platform.Platform
	Agents        map[string]session.AgentOpts
	AgentCommands map[string]string
	StorePath     string
	MaxJobs       int
	// MaxJobsPerChat overrides DefaultMaxJobsPerChat when > 0. Zero (and
	// negative) values fall back to the default — this is deliberate so
	// operators cannot accidentally disable the cap and let one chat
	// starve the exempt-session pool (see DefaultMaxJobsPerChat's BL2
	// note). R208-BL2.
	MaxJobsPerChat int
	ExecTimeout    time.Duration
	// Location is the timezone in which schedule expressions are evaluated.
	// nil defaults to time.Local so cron expressions match wall-clock time
	// on the host (respects $TZ / /etc/localtime).
	//
	// DST caveats (inherited from robfig/cron v3):
	//   - Spring-forward (hour skipped): a schedule whose expression lands in
	//     the missing hour fires zero times that day.
	//   - Fall-back (hour repeated): a schedule whose expression lands in the
	//     repeated hour may fire twice within the same wall-clock hour. Fast
	//     jobs that complete before the second trigger are not protected by
	//     SkipIfStillRunning.
	// For time-critical periodic work (billing, audit snapshots) prefer a UTC
	// Location so the schedule is immune to DST transitions.
	Location *time.Location
	// NotifyDefault provides a fallback IM target for jobs that opt into
	// notifications (Job.Notify == true) but have no per-job target set.
	// Empty Platform or ChatID disables the default.
	NotifyDefault NotifyTarget
	// ParentCtx, if set, is used as the parent for the scheduler's internal stop context.
	// When it is cancelled (e.g. during application shutdown) all running cron jobs are
	// interrupted promptly.
	ParentCtx context.Context
	// AllowedRoot mirrors Server.allowedRoot: the only directory tree under
	// which cron jobs may execute. Persisted jobs whose WorkDir falls outside
	// this root are refused at Start() load time — otherwise an attacker who
	// tampers with cron_jobs.json on disk (or a job persisted before the
	// operator configured AllowedRoot) could escape the sandbox at replay.
	// Empty disables the check (back-compat for tests and legacy deployments).
	AllowedRoot string
	// JitterMax is the upper bound of the randomized delay applied before
	// each scheduled tick. 0 disables jitter (preserves legacy behavior).
	// The per-job window is clamped to min(JitterMax, period/4) so short
	// schedules are not swallowed. TriggerNow bypasses jitter.
	// See docs/rfc/cron-v2-polish.md §3.2.
	JitterMax time.Duration
}

// NotifyTarget identifies an IM channel for cron completion notifications.
type NotifyTarget struct {
	Platform string
	ChatID   string
}

// IsSet reports whether both fields are populated.
func (n NotifyTarget) IsSet() bool { return n.Platform != "" && n.ChatID != "" }

// OnExecuteFunc is called after a cron job finishes execution.
// It receives the job ID, result text (or empty), and error message (or empty).
type OnExecuteFunc func(jobID, result, errMsg string)

// RunStartedEvent is broadcast when a cron run enters the running state
// (after CAS gate, before IM notify resolution). Consumers (Hub) marshal
// to a WS message; the cron package itself never serialises — this keeps
// the package free of server / wshub coupling.
type RunStartedEvent struct {
	JobID     string
	RunID     string
	StartedAt time.Time
	Trigger   TriggerKind
	SessionID string // 可能为空：CAS 之后立刻广播时 GetOrCreate 还没跑
	Fresh     bool
}

// RunEndedEvent is broadcast when a cron run reaches a terminal state
// (succeeded / failed / skipped / timed_out / canceled). EndedAt and
// DurationMS reflect the wall-clock that record path observes.
type RunEndedEvent struct {
	JobID      string
	RunID      string
	State      RunState
	StartedAt  time.Time
	EndedAt    time.Time
	DurationMS int64
	SessionID  string
	ErrorClass ErrorClass
	ErrorMsg   string
	Trigger    TriggerKind
}

// OnRunStartedFunc / OnRunEndedFunc are server-side hooks for WS broadcast.
// Both nil-safe; Scheduler invokes them outside s.mu so handlers may take
// hub locks without inversion risk.
type (
	OnRunStartedFunc func(RunStartedEvent)
	OnRunEndedFunc   func(RunEndedEvent)
)

// Scheduler manages cron jobs and executes them on schedule.
type Scheduler struct {
	cron *robfigcron.Cron
	mu   sync.RWMutex
	jobs map[string]*Job
	// router is set once in NewScheduler and never reassigned.
	router SessionRouter
	// platforms / agents / agentCommands are populated from SchedulerConfig
	// at NewScheduler and treated as immutable thereafter — notifyTarget
	// reads platforms without s.mu (line ~1864) and executeOpt reads agents
	// without s.mu (line ~1534). A future caller must NOT mutate these maps
	// in place; if dynamic backend/agent registration ever lands, switch to
	// atomic.Pointer[map[...]] swap-on-write so reads stay lock-free without
	// racing the writer.
	platforms     map[string]platform.Platform
	agents        map[string]session.AgentOpts
	agentCommands map[string]string
	storePath     string
	maxJobs       int
	// maxJobsPerChat is the resolved per-chat cap: SchedulerConfig
	// MaxJobsPerChat when > 0, otherwise DefaultMaxJobsPerChat. Immutable
	// after NewScheduler returns, so AddJob can read it lock-free.
	maxJobsPerChat int
	execTimeout    time.Duration
	// location is the timezone used to interpret schedule expressions and to
	// compute preview/next-run times exposed via the dashboard.
	location *time.Location
	// notifyDefault is the fallback IM target used when a job has Notify=true
	// but no per-job target; zero value means no default (then notifications
	// only flow when per-job NotifyPlatform/NotifyChatID are set).
	notifyDefault NotifyTarget
	// allowedRoot restricts job WorkDir to a filesystem subtree. Applied at
	// Start() load time to catch tampered/legacy store entries, and at
	// execute() time to catch symlink races that retarget post-creation.
	// Empty disables enforcement (tests/legacy).
	allowedRoot string
	// allowedRootResolved is a construction-time snapshot of
	// filepath.EvalSymlinks(allowedRoot), used as a best-effort fallback
	// by workDirUnderRoot when the per-call EvalSymlinks on allowedRoot
	// itself fails (e.g. the root is temporarily unmounted / missing).
	// The per-call EvalSymlinks is still the primary path so the TOCTOU
	// protection against symlink swaps on the root side is preserved.
	// Empty means no cache; workDirUnderRoot then falls back to the raw
	// allowedRoot string when its own EvalSymlinks fails (legacy
	// behaviour).
	allowedRootResolved string
	// jitterMax is the scheduling jitter cap. See SchedulerConfig.JitterMax.
	// Immutable after NewScheduler returns, so no lock needed.
	jitterMax time.Duration
	// startedAtNanos 是 Start() 被调用的时刻（UnixNano）。用于 missed-schedule 检测的启动
	// 抑制窗口——刚启动时所有长间隔 job 都会被算成"错过过"，需要
	// (now - startedAt) > 5×period 时才算 missed。原本用 time.Time 字段，
	// Start() 写入和 StartedAt() 读取走两条路径无锁同步；-race 下 dashboard
	// 启动后立刻 poll StartedAt() 会被探测为竞态。改成 atomic.Int64 (UnixNano)
	// 后写读都走 atomic Load/Store。R216-GO-3。
	startedAtNanos atomic.Int64
	// stopCtx is the scheduler's lifecycle context. Storing context in a
	// struct is usually an anti-pattern, but here execute() is invoked via
	// a callback from robfig/cron whose signature has no ctx parameter, so
	// the scheduler itself owns the root context so Stop() can cancel in-
	// flight executions. Callers outside execute() take ctx as an argument.
	stopCtx    context.Context
	stopCancel context.CancelFunc
	// R225-GO-5: callback fields accessed via atomic.Pointer so external
	// readers (emit{Started,Ended} / recordResult variants) don't need to
	// hold s.mu, and tests that read fields directly cannot race the setters
	// that previously took s.mu only during write.
	onExecute    atomic.Pointer[OnExecuteFunc]
	onRunStarted atomic.Pointer[OnRunStartedFunc]
	onRunEnded   atomic.Pointer[OnRunEndedFunc]

	// triggerWG tracks goroutines spawned by TriggerNow so Stop() can wait
	// for them to finish. The scheduled entries are already drained by
	// s.cron.Stop(), but manual TriggerNow fires a goroutine outside the
	// cron scheduler's purview.
	triggerWG sync.WaitGroup

	// runningJobs serializes execute(j) calls per job ID so a manual
	// TriggerNow cannot overlap a scheduled tick for the same job (the cron
	// chain's SkipIfStillRunning only protects the scheduled path). Entries
	// are intentionally NOT cleared on job delete — a concurrent execute()
	// may still hold the *runInflight (containing the atomic.Bool CAS gate)
	// and be about to Store(false) it; if a fresh AddJob somehow reused the
	// same ID (low but non-zero given the hex8 generator), creating a new
	// guard entry would split the CAS gate between two goroutines and permit
	// double execution. The leak is bounded by maxJobsHardCap so the trade
	// is cheap vs. a correctness gap.
	//
	// P0 (cron-run-history.md): the per-job entry was *atomic.Bool; lifted
	// to *runInflight so the CAS gate keeps its semantics while exposing
	// RunID/StartedAt/Phase/SessionID/Trigger to list handlers.
	runningJobs sync.Map // map[jobID]*runInflight

	// storeMu serialises saveSnapshot writes so the last-writer-wins order
	// matches the order snapshots were marshaled under s.mu. WriteFileAtomic
	// now uses os.CreateTemp so the underlying .tmp file is unique per call
	// and cannot be corrupted by parallel writers; storeMu remains only as
	// a logical barrier against reordering (an older snapshot rename-winning
	// over a newer one). Held only around the WriteFileAtomic call inside
	// saveMarshaledSeq — snapshot construction stays on s.mu to avoid
	// cross-lock latency.
	storeMu sync.Mutex

	// saveSeq is a monotonic sequence tag attached to every marshaled
	// snapshot at the moment persistJobsLocked captures it (under s.mu).
	// saveMarshaled consults lastSavedSeq while holding storeMu and skips
	// the WriteFileAtomic call if a concurrent writer has already landed
	// a newer snapshot. This closes R48-REL-PERSIST-ORDERING-RACE: Go
	// sync.Mutex does not guarantee FIFO acquisition, so two concurrent
	// mutations could marshal A (older, seq=1) then B (newer, seq=2) and
	// have B reach storeMu first — without the seq gate, A would then
	// overwrite B on disk. The gate makes saveMarshaled idempotent w.r.t.
	// stale payloads and eliminates the ordering window entirely.
	saveSeq      atomic.Uint64 // assigned while holding s.mu
	lastSavedSeq atomic.Uint64 // read/CAS'd while holding storeMu

	// runStore persists a CronRun record per terminal execution (P1
	// cron-run-history). nil-safe: empty StorePath disables persistence
	// transparently (tests / no-disk deployments).
	runStore *runStore
}

// SetOnExecute registers a callback invoked after each cron job execution.
func (s *Scheduler) SetOnExecute(fn OnExecuteFunc) {
	if fn == nil {
		s.onExecute.Store(nil)
		return
	}
	s.onExecute.Store(&fn)
}

// SetOnRunStarted registers a callback for the run-started broadcast event.
// nil disables the broadcast (testing path / no-WS mode).
func (s *Scheduler) SetOnRunStarted(fn OnRunStartedFunc) {
	if fn == nil {
		s.onRunStarted.Store(nil)
		return
	}
	s.onRunStarted.Store(&fn)
}

// SetOnRunEnded registers a callback for the run-ended broadcast event.
// Invoked for every terminal state including skipped/canceled — the
// callback should distinguish via RunEndedEvent.State.
func (s *Scheduler) SetOnRunEnded(fn OnRunEndedFunc) {
	if fn == nil {
		s.onRunEnded.Store(nil)
		return
	}
	s.onRunEnded.Store(&fn)
}

// CurrentRun returns the inflight snapshot for jobID, or (zero, false) when
// the job is not currently executing. Used by the dashboard list API to
// show "running 12s" badges.
func (s *Scheduler) CurrentRun(jobID string) (runInflightView, bool) {
	v, ok := s.runningJobs.Load(jobID)
	if !ok {
		return runInflightView{}, false
	}
	// Defensive: runningJobs is sync.Map[string]*runInflight by contract,
	// but the type-erased Load makes a future refactor that stores a
	// different type or a nil value silently panic here. The two-value
	// assertion + nil check turns that into a graceful "no inflight".
	inf, ok := v.(*runInflight)
	if !ok || inf == nil {
		return runInflightView{}, false
	}
	return inf.snapshot()
}

// RunInflightView is the exported shape for CurrentRun's snapshot,
// surfaced by server-side handlers building the list / detail JSON
// response. Kept here (cron package) so the field set stays single-
// sourced; the server view re-marshals into its own wire shape.
type RunInflightView = runInflightView

// ListRuns returns up to limit CronRunSummary entries for jobID, newest
// first. before is a cutoff (only runs with StartedAt < before); zero
// means "no cutoff" (latest page).
//
// Safe to call when persistence is disabled (StorePath empty): returns
// nil. The dashboard list endpoint and detail endpoint both go through
// this method so the runs/ schema stays opaque to server/.
func (s *Scheduler) ListRuns(jobID string, limit int, before time.Time) []CronRunSummary {
	if s == nil || s.runStore == nil {
		return nil
	}
	return s.runStore.List(jobID, limit, before)
}

// RecentRuns is the convenience wrapper for the cron list view's
// recent_runs field. Cap is enforced inside ListRuns.
func (s *Scheduler) RecentRuns(jobID string, n int) []CronRunSummary {
	if s == nil || s.runStore == nil {
		return nil
	}
	return s.runStore.Recent(jobID, n)
}

// GetRun returns the full CronRun for runID under jobID. Returns
// (nil, fs.ErrNotExist) when missing; (nil, ErrCorruptRun) when present
// but unusable. Server layer maps these to 404 / 500 respectively.
func (s *Scheduler) GetRun(jobID, runID string) (*CronRun, error) {
	if s == nil || s.runStore == nil {
		return nil, fs.ErrNotExist
	}
	return s.runStore.Get(jobID, runID)
}

// knownSessionIDsRecentCap bounds how many recent runs per job we walk
// when building the known-IDs set. Cron jobs share the user's workspace
// (~/.claude/projects/<workspace>/<UUID>.jsonl is co-located with regular
// dashboard sessions), so the only way to hide cron-spawned JSONLs from
// the history panel is per-session-ID. We pull `recentCap` runs per job
// — enough to cover the full history-panel window (200 entries × 7d).
// Walking the full per-job ring would reread every JSONL on every poll
// (handleList is hit at 1Hz × N tabs); ahead-of-time bounded scan
// matches the dashboard's display cap. Operators with very busy crons
// (more than recentCap distinct SessionIDs in 7 days) accept that older
// rotations may briefly resurface in history until their JSONL ages out.
const knownSessionIDsRecentCap = 200

// KnownSessionIDs returns the set of Claude session IDs (UUID-style)
// that have been spawned by cron jobs known to this Scheduler.  The
// dashboard history panel uses this as a session-ID blacklist so
// cron-spawned JSONLs are hidden from the catch-all "recent sessions"
// list (cron has its own 「定时任务」panel for inspection).
//
// Sources, in order of cost:
//
//   - All Job.LastSessionID values held in s.jobs (one per job, cheap).
//   - All in-flight runs (s.runningJobs sync.Map; one per active run).
//   - The last knownSessionIDsRecentCap runs per job from runStore.
//
// Result is a fresh map; safe to retain.  Cost is O(jobs ×
// knownSessionIDsRecentCap), bounded by maxJobsHardCap (500) ×
// recentCap (200) = 100k map ops worst case — acceptable for a
// 30-second-cached dashboard call.  Returns an empty (non-nil) map
// when there are no jobs.
//
// Safe to call on a nil Scheduler — returns empty map.  R245-ARCH
// (cron+sys hide-from-history).
func (s *Scheduler) KnownSessionIDs() map[string]bool {
	if s == nil {
		return map[string]bool{}
	}
	out := make(map[string]bool, 32)

	s.mu.RLock()
	jobIDs := make([]string, 0, len(s.jobs))
	for id, j := range s.jobs {
		jobIDs = append(jobIDs, id)
		if j.LastSessionID != "" {
			out[j.LastSessionID] = true
		}
	}
	s.mu.RUnlock()

	// In-flight runs may have a SessionID set even before the run
	// terminates (set by setSessionID after GetOrCreate returns).
	s.runningJobs.Range(func(_, v any) bool {
		if inf, ok := v.(*runInflight); ok && inf != nil {
			if view, running := inf.snapshot(); running && view.SessionID != "" {
				out[view.SessionID] = true
			}
		}
		return true
	})

	// Persisted history.  Walk recent runs per job (already cached
	// inside runStore).  RunStore is nil only in tests.
	if s.runStore != nil {
		for _, jobID := range jobIDs {
			for _, sum := range s.runStore.Recent(jobID, knownSessionIDsRecentCap) {
				if sum.SessionID != "" {
					out[sum.SessionID] = true
				}
			}
		}
	}

	return out
}

// maxJobsHardCap caps user-configurable MaxJobs to prevent accidental
// overload. 500 jobs ≈ 500 tick timers; well within robfig/cron's tested
// scale, but higher values tend to indicate a config mistake.
const maxJobsHardCap = 500

// DefaultMaxJobsPerChat bounds how many cron jobs a single chat (platform+
// chat_id pair) may own. Prevents one loud group from consuming the
// global MaxJobs quota. Exported so tests and docs can reference the
// value; operators can override per deployment via
// SchedulerConfig.MaxJobsPerChat (zero / unset falls back to this
// default — no way to "disable" the cap without rebuilding).
//
// Relationship to exempt pool (BL2 acknowledged design):
// Every cron job calls session.Router.RegisterCronStub at scheduler
// Start / AddJob time and consumes 1 slot from session.maxExemptSessions
// (currently 20). At DefaultMaxJobsPerChat=10 × 2 busy chats, the exempt
// pool is fully consumed and planner/scratch exempt sessions may be
// starved. This is an acknowledged trade-off: a separate
// maxCronExemptSessions reserve or per-chat fair-share eviction is the
// escape hatch if pressure materialises.
const DefaultMaxJobsPerChat = 10

// cronSlowThreshold is the wall-clock budget beyond which a successful
// cron execution is counted as "slow" (metrics.CronExecutionSlowTotal).
// 30s is picked as an order-of-magnitude above a typical interactive
// agent turn; jobs that regularly tip over are candidates for timeout /
// workflow inspection. R208-OBS1.
const cronSlowThreshold = 30 * time.Second

// workDirReachable reports whether workDir exists and resolves to a
// directory right now. Used before fresh-mode Reset so a job whose
// workspace has been deleted by an operator does not destroy the
// existing session just to fail on a GetOrCreate / spawn-shim call.
// Empty workDir means "use router default" and is always reachable.
// CRON2.
func workDirReachable(workDir string) bool {
	if workDir == "" {
		return true
	}
	info, err := os.Stat(workDir)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// workDirUnderRoot reports whether workDir resolves (after symlink evaluation)
// to a path at or under allowedRoot. EvalSymlinks is done per-call for both
// sides so the check reflects current filesystem state — this closes the
// TOCTOU window between creation-time validateWorkspace and execute-time
// workspace binding AND the separate window where allowedRoot itself (if a
// symlink) could be retargeted after construction. Both arguments must be
// absolute; relative workDir is rejected. allowedRootResolved, when
// non-empty, is a best-effort prior resolution of allowedRoot that is used
// as a fallback only if the per-call EvalSymlinks on allowedRoot itself
// fails (e.g. the path was temporarily unmounted). This preserves the
// security contract while still avoiding most of the syscall cost of a
// cold re-resolution on the happy path.
func workDirUnderRoot(workDir, allowedRoot, allowedRootResolved string) bool {
	if workDir == "" || allowedRoot == "" {
		return true // empty WorkDir uses router default; empty root = disabled
	}
	if !filepath.IsAbs(workDir) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		// Missing directory / permission denied — refuse to execute rather
		// than silently re-create the sandbox escape.
		return false
	}
	rootResolved, err := filepath.EvalSymlinks(allowedRoot)
	if err != nil {
		// Fall back to the cached resolution (captured at construction) or
		// the raw path if no cache exists. Either way the fallback chain
		// preserves the historical behaviour when EvalSymlinks fails.
		if allowedRootResolved != "" {
			rootResolved = allowedRootResolved
		} else {
			rootResolved = allowedRoot
		}
	}
	if resolved == rootResolved {
		return true
	}
	return strings.HasPrefix(resolved, rootResolved+string(filepath.Separator))
}

// NewScheduler creates a scheduler. Call Start() to begin.
func NewScheduler(cfg SchedulerConfig) *Scheduler {
	if cfg.MaxJobs <= 0 {
		cfg.MaxJobs = 50
	}
	if cfg.MaxJobs > maxJobsHardCap {
		slog.Warn("cron max_jobs exceeds hard cap, clamping", "requested", cfg.MaxJobs, "cap", maxJobsHardCap)
		cfg.MaxJobs = maxJobsHardCap
	}
	// Resolve per-chat cap at construction: <= 0 maps to the default so a
	// zero struct field cannot silently disable the cap. R208-BL2.
	maxPerChat := cfg.MaxJobsPerChat
	if maxPerChat <= 0 {
		maxPerChat = DefaultMaxJobsPerChat
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = 5 * time.Minute
	}
	parent := cfg.ParentCtx
	if parent == nil {
		parent = context.Background()
	}
	stopCtx, stopCancel := context.WithCancel(parent)
	// Resolve the allowed root once at construction; subsequent workDir
	// checks skip the syscall chain for the root side. Empty result falls
	// back to lazy resolution per-call.
	var allowedRootResolved string
	if cfg.AllowedRoot != "" {
		if r, err := filepath.EvalSymlinks(cfg.AllowedRoot); err == nil {
			allowedRootResolved = r
		}
	}
	cronLogger := robfigcron.PrintfLogger(slogPrintfLogger{})
	loc := cfg.Location
	if loc == nil {
		loc = time.Local
	}
	return &Scheduler{
		cron: robfigcron.New(
			robfigcron.WithLocation(loc),
			robfigcron.WithChain(
				robfigcron.Recover(cronLogger),
				robfigcron.SkipIfStillRunning(cronLogger),
			),
		),
		jobs:                make(map[string]*Job),
		router:              cfg.Router,
		platforms:           cfg.Platforms,
		agents:              cfg.Agents,
		agentCommands:       cfg.AgentCommands,
		storePath:           cfg.StorePath,
		maxJobs:             cfg.MaxJobs,
		maxJobsPerChat:      maxPerChat,
		execTimeout:         cfg.ExecTimeout,
		location:            loc,
		notifyDefault:       cfg.NotifyDefault,
		allowedRoot:         cfg.AllowedRoot,
		allowedRootResolved: allowedRootResolved,
		jitterMax:           cfg.JitterMax,
		stopCtx:             stopCtx,
		stopCancel:          stopCancel,
		runStore:            newRunStore(cfg.StorePath, 0, 0),
	}
}

// NotifyDefault returns the configured fallback IM target so the dashboard can
// show users where a "notify on completion" toggle will deliver messages.
func (s *Scheduler) NotifyDefault() NotifyTarget { return s.notifyDefault }

// StartedAt 返回 Scheduler 最近一次 Start() 的时刻。用于 missed-schedule
// 检测的启动抑制窗口。未 Start 前返回零值。
func (s *Scheduler) StartedAt() time.Time {
	ns := s.startedAtNanos.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// Start loads persisted jobs and starts the cron scheduler.
func (s *Scheduler) Start() error {
	// 记录启动时刻，missed-schedule 检测靠它做启动抑制窗口（见
	// HasMissedSchedule）。写在 loadJobs 之前保证即使 loadJobs 失败 StartedAt
	// 也不被污染——失败时 Start 提前返回，下次重试会覆盖。
	s.startedAtNanos.Store(time.Now().UnixNano())

	// loadJobs distinguishes three outcomes: (map, nil) normal, (nil, nil)
	// corrupt-but-rescued, (nil, error) original file still on disk. In the
	// error case we must refuse to start — otherwise the first subsequent
	// persist would overwrite the operator's real jobs with `[]`, silently
	// losing data that is still recoverable from the preserved file.
	restored, err := loadJobs(s.storePath)
	if err != nil {
		return fmt.Errorf("load cron store: %w", err)
	}

	s.mu.Lock()
	// Snapshot the fields we pass to registerStub under lock so we don't
	// dereference *Job after releasing s.mu — once cron.Start() fires, any
	// future UpdateJob could race with a stub read via the map pointer.
	// lastSessionID 跟其它字段一起快照，这样重启后恢复的 cron stub 仍然
	// 带上上次成功执行留下的 session_id，historySource 才能从 JSONL 把
	// 历史读回来给 dashboard 显示。
	type stubRow struct{ id, workDir, prompt, lastSessionID string }
	var stubs []stubRow
	for _, j := range restored {
		// Reject persisted jobs whose WorkDir escapes the configured
		// sandbox. Replaying an on-disk tampered entry must not grant
		// filesystem access that validateWorkspace would reject at
		// creation. When allowedRoot is empty (tests), this is a no-op.
		if s.allowedRoot != "" && j.WorkDir != "" && !workDirUnderRoot(j.WorkDir, s.allowedRoot, s.allowedRootResolved) {
			slog.Warn("cron job work_dir outside allowed_root; skipping",
				"id", j.ID, "work_dir", j.WorkDir)
			continue
		}
		if j.Paused {
			s.jobs[j.ID] = j
			stubs = append(stubs, stubRow{j.ID, j.WorkDir, j.Prompt, j.LastSessionID})
			continue
		}
		if err := s.registerJob(j); err != nil {
			slog.Warn("skip invalid cron job", "id", j.ID, "schedule", j.Schedule, "err", err)
			continue
		}
		s.jobs[j.ID] = j
		stubs = append(stubs, stubRow{j.ID, j.WorkDir, j.Prompt, j.LastSessionID})
	}
	jobCount := len(s.jobs)
	s.mu.Unlock()
	// Register dashboard stub sessions after releasing the lock; the router's
	// notifyChange callback must not re-enter scheduler state. Use snapshotted
	// values (not the *Job pointer) so a concurrent UpdateJob mutating the map
	// entry cannot race with our reads.
	for _, st := range stubs {
		s.registerStubByValue(st.id, st.workDir, st.prompt, st.lastSessionID)
	}
	s.cron.Start()
	// P1 cron-run-history: cold-start GC pass over 'runs/' tree to collect
	// retention-policy violators that accumulated while this process was
	// down. 异步执行避免在 jobs 多/历史目录大时阻塞 Start 返回（每个 job
	// 一次 ReadDir + N 次 Remove）。
	if s.runStore != nil {
		go func() {
			slog.Info("cron run history: cold-start GC starting")
			s.runStore.trimAll(time.Now())
			slog.Info("cron run history: cold-start GC done")
		}()
	}
	slog.Info("cron scheduler started", "jobs", jobCount)
	return nil
}

// registerStub creates (or refreshes) a router session entry for the job so it
// appears in the dashboard workspace list. Safe to call without a router (tests).
// Callers must not be holding s.mu — RegisterCronStubWithChain re-enters router state.
//
// 当 job 存了 LastSessionID（最近一次成功执行的 session_id），会把它
// 作为单元素 chain 传给 stub，这样 dashboard 点击 cron 侧边栏时能按
// 该 ID 从 claude 项目目录找到 JSONL 历史。否则 fresh_context=true 的
// 定时任务每次 Reset 都会把 stub 的 chain 清空，事件面板就永远是空白。
func (s *Scheduler) registerStub(j *Job) {
	if s.router == nil {
		return
	}
	s.router.RegisterCronStubWithChain(session.CronKey(j.ID), j.WorkDir, j.Prompt, stubChain(j.LastSessionID))
}

// registerStubByValue is the pointer-free variant used from Start() where the
// caller has already snapshotted mutable fields under s.mu.
func (s *Scheduler) registerStubByValue(id, workDir, prompt, lastSessionID string) {
	if s.router == nil {
		return
	}
	s.router.RegisterCronStubWithChain(session.CronKey(id), workDir, prompt, stubChain(lastSessionID))
}

// stubChain returns the single-element resume chain for a cron stub when
// LastSessionID is set, or nil otherwise. Centralises the chain-building
// shared by registerStub / registerStubByValue.
func stubChain(lastSessionID string) []string {
	if lastSessionID == "" {
		return nil
	}
	return []string{lastSessionID}
}

// EnsureStub lazily (re-)registers a dashboard stub session for the given
// key (format "cron:<jobID>"). Returns true when the matching job still
// exists and a stub is now registered (either newly created or already
// present); returns false when the key is malformed, not a cron key, or
// the job is gone.
//
// Rationale: the sidebar "×" button routes through router.Remove and
// deletes the stub. Cron stubs are meant to be re-bornable — the next
// scheduled tick rebuilds them via executeJob's GetOrCreate — but between
// the dismissal and that tick, clicking the task card in the Cron panel
// would otherwise hit "session not found" because the WS subscribe path
// has nothing to attach to. This method is the idempotent recovery hook
// wired into handleSubscribe and /api/sessions/events.
func (s *Scheduler) EnsureStub(key string) bool {
	if !session.IsCronKey(key) {
		return false
	}
	id := key[len(session.CronKeyPrefix):]
	if id == "" {
		return false
	}
	// Snapshot workDir/prompt under RLock, release before reaching into
	// router: RegisterCronStubWithChain calls notifyChange which fans out to
	// hub broadcasters, and holding s.mu across that path risks lock-order
	// inversion with the cron dispatcher (see ListAllJobsWithNextRun).
	s.mu.RLock()
	j, ok := s.jobs[id]
	var workDir, prompt, lastSessionID string
	if ok {
		workDir = j.WorkDir
		prompt = j.Prompt
		lastSessionID = j.LastSessionID
	}
	s.mu.RUnlock()
	if !ok {
		return false
	}
	s.registerStubByValue(id, workDir, prompt, lastSessionID)
	return true
}

// stopBudget is the overall deadline Scheduler.Stop() will spend waiting on
// cron.Stop + triggerWG before proceeding to save. Shared between both waits
// (not doubled per wait) so a production deployment with execTimeout=3600s
// cannot pin restart for ≈2 h — the prior two-budget design had a worst case
// of 2×(execTimeout+5s). Aligned with session.ShutdownTimeout (30s) so both
// subsystems agree on the upper bound systemd sees.
//
// Package-level var (not const) so tests can shorten it to milliseconds
// without race-racing a Stop call with real wall-clock timeouts.
// R49-REL-CRON-STOP-BUDGET.
var stopBudget = 30 * time.Second

// Stop halts the scheduler and saves state. It waits for both scheduled jobs
// (drained by s.cron.Stop) and any TriggerNow-spawned goroutines before
// returning, so callers can safely tear down the router afterwards.
//
// Shutdown is bounded by stopBudget (30s by default). A stuck cron job
// (execute() hanging past ctx cancel, e.g. a broken shim ignoring context)
// or a stuck triggerWG (deliverNotice → platform Reply webhook that refuses
// to honour its own timeout) cannot hold us past this budget. The final
// saveJobs runs regardless so a stuck drain does not cost the state file.
//
// CONTRACT: Stop assumes the naozhi process terminates shortly after it
// returns. When triggerWG.Wait is cut off by the budget, the wrapper
// goroutine around it is intentionally leaked — the deliverNotice that
// holds it is typically blocked on a hung platform webhook, and the only
// way to reclaim it is to let the OS tear the process down. This is
// acceptable precisely because Scheduler is not reusable: there is no
// path that calls Stop() and then constructs new cron work on the same
// instance. If you ever add one, you MUST replace the bare wrapper with
// a ctx-aware pattern and reclaim the goroutine, otherwise restart
// cycles accumulate stuck webhook goroutines until OOM. R44-REL-
// TRIGGER-GOROUTINE.
func (s *Scheduler) Stop() {
	s.stopCancel()
	cronDoneCtx := s.cron.Stop()

	// Single overall deadline shared across both waits. If cron.Stop drains
	// fast we have the full budget for triggerWG; if it eats the budget we
	// skip triggerWG.Wait entirely (the leaked goroutines die when the
	// process exits moments later). Either way saveJobs runs — losing it
	// would undo mutations that had already returned 2xx to dashboard/IM.
	deadline := time.NewTimer(stopBudget)
	defer deadline.Stop()

	deadlineHit := false
	select {
	case <-cronDoneCtx.Done():
	case <-deadline.C:
		deadlineHit = true
		slog.Warn("cron scheduler: stop deadline exceeded before cron.Stop drained, proceeding",
			"budget", stopBudget)
	}

	// Bound triggerWG.Wait with the *remaining* share of the same budget:
	// while manual TriggerNow respects stopCtx via execute(), the notify
	// delivery path (deliverNotice → platform Reply) uses a Background-
	// derived ctx so stopCtx cancellation does not interrupt an in-flight
	// webhook POST. Without this deadline, a stuck platform HTTP call
	// could pin Stop() past systemd TimeoutStopSec.
	//
	// R222-GO-10: when the deadline pre-empts triggerDone, the wrapper
	// goroutine started by `go func() { s.triggerWG.Wait(); close(...) }`
	// stays parked on triggerWG.Wait — exactly the intentional-orphan path
	// documented in the function-level CONTRACT block (lines 715–725). The
	// cost is one wedged goroutine per process exit; reclaim happens when
	// the OS tears the process down moments later. We deliberately do NOT
	// add a sync.Once / chan-cancel reclaim path here: triggerWG.Wait does
	// not accept a cancel signal, and Scheduler is single-shot (Stop is
	// terminal). The `if !deadlineHit` outer gate already keeps us from
	// spawning the wrapper when cron.Stop itself ate the budget. A
	// goroutine-leak detector running in tests that shorten stopBudget to
	// milliseconds will surface this orphan; tests that care should plumb
	// a non-stuck deliverNotice fake instead.
	if !deadlineHit {
		triggerDone := make(chan struct{})
		go func() {
			s.triggerWG.Wait()
			close(triggerDone)
		}()
		select {
		case <-triggerDone:
		case <-deadline.C:
			slog.Warn("cron scheduler: stop deadline exceeded during triggerWG wait, proceeding",
				"budget", stopBudget)
		}
	}

	s.mu.Lock()
	data, err := s.marshalJobsLocked()
	s.mu.Unlock()
	if err != nil {
		slog.Error("marshal cron store on shutdown", "err", err)
		return
	}
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	if s.storePath == "" {
		return
	}
	if err := osutil.WriteFileAtomic(s.storePath, data, 0600); err != nil {
		slog.Error("save cron store on shutdown", "err", err, "disk_full", osutil.IsDiskFull(err))
	}
}

// AddJob validates, registers, and persists a new cron job.
func (s *Scheduler) AddJob(j *Job) error {
	if err := validateSchedule(j.Schedule); err != nil {
		return fmt.Errorf("invalid schedule %q: %w", j.Schedule, err)
	}
	// Title 长度校验在 scheduler 层兜底，避免绕过 dashboard handler（例如
	// store 直接加载被篡改的 cron_jobs.json）把超长字符串持久化进内存。
	if n := utf8.RuneCountInString(j.Title); n > MaxCronTitleLen {
		return fmt.Errorf("title too long: %d runes > %d cap", n, MaxCronTitleLen)
	}

	// addJobLocked runs under s.mu (defer Unlock). Splitting the locked
	// section into a helper means every early-return path goes through
	// defer and removes the prior pattern of 4 manual s.mu.Unlock() calls
	// (R228-GO-2): adding a new validation step inside the locked section
	// no longer risks leaking a held mutex on the new error path.
	save, perr := s.addJobLocked(j)
	if perr != nil {
		// addJobLocked may surface either a pre-mutation error (capacity
		// rejection — no save returned) or a post-mutation persist error
		// (in-memory insertion already happened). The caller cannot tell
		// the two apart from the error alone, but in either case there
		// is no save() to invoke — addJobLocked returns nil for save in
		// both branches.
		return perr
	}
	save()
	s.registerStub(j)
	return nil
}

// addJobLocked performs the AddJob mutation under s.mu. Returns the
// post-unlock save closure (nil on error) and any error. Split from
// AddJob so a single defer handles unlock across all early-return
// paths. R228-GO-2.
func (s *Scheduler) addJobLocked(j *Job) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.jobs) >= s.maxJobs {
		return nil, fmt.Errorf("max cron jobs reached (%d)", s.maxJobs)
	}

	// Per-chat limit to prevent one chat from exhausting global quota.
	// O(maxJobs) linear scan; acceptable given maxJobsHardCap=500 and
	// AddJob is called at human cadence (not on the hot path). A
	// chatID-indexed map would mirror the sessionsByChat optimisation in
	// the router but is premature given the bound.
	chatCount := 0
	for _, existing := range s.jobs {
		if existing.Platform == j.Platform && existing.ChatID == j.ChatID {
			chatCount++
		}
	}
	if chatCount >= s.maxJobsPerChat {
		return nil, fmt.Errorf("per-chat cron limit reached (%d)", s.maxJobsPerChat)
	}

	j.ID = generateID()
	// Retry on unlikely ID collision.
	for _, exists := s.jobs[j.ID]; exists; _, exists = s.jobs[j.ID] {
		j.ID = generateID()
	}
	j.CreatedAt = time.Now()

	if !j.Paused {
		if err := s.registerJob(j); err != nil {
			return nil, err
		}
	}
	s.jobs[j.ID] = j
	save, perr := s.persistJobsLocked()
	if perr != nil {
		// In-memory insertion + cron scheduling already happened; we
		// cannot roll those back safely (another goroutine may have
		// observed the registered entry). Surface the error so the
		// HTTP layer returns a 500 and the operator sees the
		// persistence gap.
		return nil, perr
	}
	return save, nil
}

// ListJobs returns jobs for a specific chat.
func (s *Scheduler) ListJobs(plat, chatID string) []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []Job
	for _, j := range s.jobs {
		if j.Platform == plat && j.ChatID == chatID {
			result = append(result, *j)
		}
	}
	return result
}

// JobWithNextRun pairs a Job snapshot with its next scheduled run time so
// callers rendering lists (dashboard) don't need a second round-trip per job.
type JobWithNextRun struct {
	Job     Job
	NextRun time.Time
}

// ListAllJobsWithNextRun returns every job plus its next scheduled run.
// Lock strategy: snapshot (*Job, entryID) under s.mu.RLock, release s.mu, then
// call s.cron.Entry() without holding s.mu. Calling cron.Entry inside s.mu
// would invert the lock order taken by the cron dispatcher path
// (cron-internal → execute → recordResult → s.mu.Lock), risking a deadlock.
func (s *Scheduler) ListAllJobsWithNextRun() []JobWithNextRun {
	s.mu.RLock()
	type pair struct {
		job     Job
		entryID robfigcron.EntryID
	}
	pairs := make([]pair, 0, len(s.jobs))
	for _, j := range s.jobs {
		pairs = append(pairs, pair{job: *j, entryID: j.entryID})
	}
	s.mu.RUnlock()

	result := make([]JobWithNextRun, 0, len(pairs))
	for _, p := range pairs {
		var next time.Time
		if p.entryID != 0 {
			next = s.cron.Entry(p.entryID).Next
		}
		result = append(result, JobWithNextRun{Job: p.job, NextRun: next})
	}
	return result
}

// deleteJobLocked performs the in-memory side effects of removing a job:
// stop the cron entry, reset the bound session (also evicts the cron
// session from the router so the dashboard sidebar stub is removed),
// and drop the map entry.
//
// Caller must hold s.mu.Lock() and pass a non-nil job that exists in
// s.jobs. Intentionally does NOT delete from s.runningJobs: a concurrent
// execute() for this job may still hold the atomic.Bool and be about to
// CAS it back to false; if a fresh AddJob somehow reused the same ID
// (low but non-zero given the hex8 generator), creating a new guard entry
// here could split the CAS gate between two goroutines and permit double
// execution. Retaining the entry is bounded by maxJobsHardCap (one
// *atomic.Bool per historical job) — cheap vs a correctness gap. R219-CR-4.
func (s *Scheduler) deleteJobLocked(j *Job) {
	if j.entryID != 0 {
		s.cron.Remove(j.entryID)
	}
	if s.router != nil {
		s.router.Reset(session.CronKey(j.ID))
	}
	delete(s.jobs, j.ID)
}

// pauseJobLocked transitions a job to Paused state under s.mu. Returns
// ErrJobAlreadyPaused without mutation if the job is already paused so
// the caller can map it to 409 Conflict. R219-CR-4.
func (s *Scheduler) pauseJobLocked(j *Job) error {
	if j.Paused {
		return fmt.Errorf("%w: id %q", ErrJobAlreadyPaused, j.ID)
	}
	if j.entryID != 0 {
		s.cron.Remove(j.entryID)
		j.entryID = 0
	}
	j.Paused = true
	return nil
}

// resumeJobLocked transitions a paused job back to active under s.mu by
// re-registering the cron entry. Returns ErrJobNotPaused without mutation
// if the job is not paused, or registerJob's error if re-registration
// fails (e.g. schedule no longer parses) — leaving Paused=true so the
// caller can retry. R219-CR-4.
func (s *Scheduler) resumeJobLocked(j *Job) error {
	if !j.Paused {
		return fmt.Errorf("%w: id %q", ErrJobNotPaused, j.ID)
	}
	if err := s.registerJob(j); err != nil {
		return err
	}
	j.Paused = false
	return nil
}

// DeleteJobByID removes a job by exact ID (unscoped, for dashboard use).
func (s *Scheduler) DeleteJobByID(id string) (*Job, error) {
	s.mu.Lock()
	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	}
	s.deleteJobLocked(j)
	save, perr := s.persistJobsLocked()
	s.mu.Unlock()

	if perr != nil {
		return nil, perr
	}
	save()
	// P1 cron-run-history: drop the runs/<jobID>/ subtree alongside the
	// job entry. Does NOT touch ~/.claude/projects/<cwd>/<session_id>.jsonl
	// (RFC §2.3 / §4.4): those JSONL files are user-facing claude session
	// logs, deletable only via session.Router or the user's own claude
	// commands.
	if s.runStore != nil {
		s.runStore.DeleteJob(j.ID)
	}
	return j, nil
}

// PauseJobByID pauses a job by exact ID (unscoped, for dashboard use).
func (s *Scheduler) PauseJobByID(id string) (*Job, error) {
	s.mu.Lock()
	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	}
	if err := s.pauseJobLocked(j); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	save, perr := s.persistJobsLocked()
	s.mu.Unlock()

	if perr != nil {
		return nil, perr
	}
	save()
	return j, nil
}

// ResumeJobByID resumes a paused job by exact ID (unscoped, for dashboard use).
func (s *Scheduler) ResumeJobByID(id string) (*Job, error) {
	s.mu.Lock()
	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	}
	if err := s.resumeJobLocked(j); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	save, perr := s.persistJobsLocked()
	s.mu.Unlock()

	if perr != nil {
		return nil, perr
	}
	save()
	return j, nil
}

// JobUpdate captures fields a dashboard user may edit on an existing cron
// job. Only non-nil pointers are applied, so callers can update a single
// field without resending the rest.
type JobUpdate struct {
	Schedule *string
	Prompt   *string
	WorkDir  *string
	// Notify sets Job.Notify when non-nil. nil leaves the field unchanged;
	// pointer-to-true/false writes the explicit tri-state. There's no API
	// to reset back to legacy-default (nil) once a value is set — callers
	// typically toggle between true and false instead.
	Notify *bool
	// NotifyPlatform / NotifyChatID behave like Prompt / WorkDir: nil keeps
	// the existing value, a pointer to "" clears it.
	NotifyPlatform *string
	NotifyChatID   *string
	// FreshContext toggles whether each run resets the session before
	// executing. nil leaves existing behavior unchanged.
	FreshContext *bool
	// Title 是人类可读名称。nil 保持原值；pointer 到 "" 会清空
	// （UI 侧回退到 Prompt 首行）。长度由 handler 层先行校验。
	Title *string
	// Backend 是 CLI backend ID（Sprint 6c, docs/rfc/multi-backend.md §9）。
	// nil 保持原值；pointer 到 "" 显式清空，回落到 router default。
	// 字符/长度由 dashboard handler 的 validateCronBackend 先行把关；
	// 未知 backend 不在此处拒绝（router wrapperFor 会 fallback）。
	Backend *string
}

// UpdateJob applies a partial edit to an existing cron job. Schedule changes
// are validated and re-registered atomically (the old robfig entry is
// removed before the new one is installed) so a failed reschedule leaves
// the previous behavior intact. Prompt/WorkDir changes flow through to the
// router stub so the dashboard sidebar reflects the edit immediately.
func (s *Scheduler) UpdateJob(id string, upd JobUpdate) (*Job, error) {
	// Validate schedule first (no lock needed) so we fail fast on bad input.
	if upd.Schedule != nil {
		if *upd.Schedule == "" {
			return nil, fmt.Errorf("schedule must not be empty")
		}
		if err := validateSchedule(*upd.Schedule); err != nil {
			return nil, fmt.Errorf("invalid schedule %q: %w", *upd.Schedule, err)
		}
	}
	// Validate WorkDir against allowedRoot here (lock-free) so dashboard
	// edits fail fast with a clear error instead of silently persisting a
	// path that execute() will later refuse at runtime. AddJob's creation
	// path applies the same check; UpdateJob previously skipped it.
	if upd.WorkDir != nil && *upd.WorkDir != "" && s.allowedRoot != "" {
		if !workDirUnderRoot(*upd.WorkDir, s.allowedRoot, s.allowedRootResolved) {
			return nil, fmt.Errorf("work_dir outside allowed root")
		}
	}
	if upd.Title != nil {
		if n := utf8.RuneCountInString(*upd.Title); n > MaxCronTitleLen {
			return nil, fmt.Errorf("title too long: %d runes > %d cap", n, MaxCronTitleLen)
		}
	}

	s.mu.Lock()
	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	}

	if upd.Prompt != nil {
		j.Prompt = *upd.Prompt
	}
	if upd.WorkDir != nil {
		// WorkDir 一换 LastSessionID 就失效：claude JSONL 按 cwd 归档，
		// 用老 workspace 的 session_id 去新 cwd 下查 history 只会 Stat 落空。
		// 清零后下次执行写入的新 SessionID 会自然属于新 workspace。
		//
		// 对比靠原生字符串相等，依赖 dashboard / AddJob 路径已对 WorkDir 做
		// 归一化（filepath.Clean / validateWorkspace）。如果将来有新 caller
		// 绕过归一化直接塞相对路径，会导致清零误判：合法但路径写法不同的
		// 相同 workspace 会被判定为变更而清零，后果是用户需要重跑一次才
		// 能恢复 chain，不致数据损坏。
		if *upd.WorkDir != j.WorkDir {
			j.LastSessionID = ""
		}
		j.WorkDir = *upd.WorkDir
	}
	if upd.Notify != nil {
		v := *upd.Notify
		j.Notify = &v
	}
	if upd.NotifyPlatform != nil {
		j.NotifyPlatform = *upd.NotifyPlatform
	}
	if upd.NotifyChatID != nil {
		j.NotifyChatID = *upd.NotifyChatID
	}
	if upd.FreshContext != nil {
		j.FreshContext = *upd.FreshContext
	}
	if upd.Title != nil {
		j.Title = *upd.Title
	}
	if upd.Backend != nil {
		j.Backend = *upd.Backend
	}

	if upd.Schedule != nil && *upd.Schedule != j.Schedule {
		j.Schedule = *upd.Schedule
		// Re-register with the new schedule unless paused (paused jobs have
		// no live entry; ResumeJob will register with the new schedule).
		if !j.Paused {
			if j.entryID != 0 {
				s.cron.Remove(j.entryID)
				j.entryID = 0
			}
			if err := s.registerJob(j); err != nil {
				s.mu.Unlock()
				return nil, fmt.Errorf("re-register cron: %w", err)
			}
		}
	}

	save, perr := s.persistJobsLocked()
	// Value-copy while still under lock so the caller sees a stable result
	// even if another goroutine mutates the job right after we unlock.
	result := *j
	s.mu.Unlock()

	if perr != nil {
		return nil, perr
	}
	save()
	// Pass the snapshotted value (via result) to registerStub so a concurrent
	// SetJobPrompt cannot tear the Prompt/WorkDir pointers we read.
	s.registerStub(&result)
	slog.Info("cron job updated", "id", id,
		"schedule_changed", upd.Schedule != nil,
		"prompt_changed", upd.Prompt != nil,
		"workdir_changed", upd.WorkDir != nil,
		"fresh_context_changed", upd.FreshContext != nil)
	return &result, nil
}

// SetJobPrompt updates a job's prompt. If the job was paused with an empty
// prompt (created from dashboard), it also unpauses and registers the schedule.
func (s *Scheduler) SetJobPrompt(id, prompt string) error {
	if prompt == "" {
		return fmt.Errorf("prompt must not be empty")
	}

	s.mu.Lock()

	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	}
	if j.Prompt != "" {
		s.mu.Unlock()
		return nil // already has a prompt, no-op
	}

	j.Prompt = prompt
	if j.Paused {
		// Delegate unpause to the shared helper so the registerJob + Paused
		// flag transition stays consistent with PauseJob/ResumeJob/UpdateJob
		// paths. R226-CR-16.
		if err := s.resumeJobLocked(j); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	save, perr := s.persistJobsLocked()
	s.mu.Unlock()

	if perr != nil {
		return perr
	}
	save()
	slog.Info("cron job prompt set", "id", id, "prompt_len", len(prompt))
	return nil
}

// previewLocation returns the timezone the preview helpers should evaluate
// schedules in. Centralised so the nil-Scheduler fallback (tests / dashboard
// bootstrap before scheduler wiring) and the live scheduler path share a
// single decision point. R219-CR-6.
//
//   - nil receiver → UTC (deterministic across machines, matches the godoc
//     contract historically published on the package-level PreviewSchedule)
//   - non-nil receiver with unset location → time.Local (legacy behaviour
//     when location was never configured; preserved to avoid subtle drift
//     in operator-facing tooling)
//   - configured location wins
func (s *Scheduler) previewLocation() *time.Location {
	if s == nil {
		return time.UTC
	}
	if s.location == nil {
		return time.Local
	}
	return s.location
}

// PreviewSchedule validates a schedule expression and returns the next run
// time. Safe to call on a nil *Scheduler — that path computes in UTC for
// tests / dashboard bootstrap before the scheduler is wired. R219-CR-6:
// previously a free-standing cron.PreviewSchedule existed for this nil
// fallback, and operators had to remember which surface to call; folded
// into the method so callers always invoke (*Scheduler).PreviewSchedule.
func (s *Scheduler) PreviewSchedule(schedule string) (time.Time, error) {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(time.Now().In(s.previewLocation())), nil
}

// PreviewScheduleN returns the next n run times for a schedule expression, in
// the scheduler's configured timezone. Used by the dashboard to preview what
// "接下来会在这些时间运行" looks like before a user commits to a frequency.
// Callers get a validation error on the first Parse failure; n is clamped to
// a sane range by the caller.
//
// Safe to call on a nil *Scheduler — same fallback as PreviewSchedule
// (UTC). R219-CR-6.
func (s *Scheduler) PreviewScheduleN(schedule string, n int) ([]time.Time, error) {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return nil, err
	}
	if n <= 0 {
		n = 1
	}
	out := make([]time.Time, 0, n)
	t := time.Now().In(s.previewLocation())
	for i := 0; i < n; i++ {
		t = sched.Next(t)
		out = append(out, t)
	}
	return out, nil
}

// Location returns the timezone the scheduler uses to evaluate cron
// expressions, so the dashboard can surface it alongside preview/next-run
// timestamps.
//
// Safe to call on a nil *Scheduler: returns UTC (matches previewLocation's
// nil branch so dashboard preview / Location calls stay in agreement during
// the bootstrap window when scheduler is not yet wired). R219-CR-6.
func (s *Scheduler) Location() *time.Location {
	if s == nil {
		return time.UTC
	}
	if s.location == nil {
		return time.Local
	}
	return s.location
}

// DeleteJob removes a job by ID prefix (scoped to the given chat).
func (s *Scheduler) DeleteJob(idPrefix, plat, chatID string) (*Job, error) {
	s.mu.Lock()
	j, err := s.findByPrefix(idPrefix, plat, chatID)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.deleteJobLocked(j)
	save, perr := s.persistJobsLocked()
	s.mu.Unlock()

	if perr != nil {
		return nil, perr
	}
	save()
	if s.runStore != nil {
		s.runStore.DeleteJob(j.ID)
	}
	return j, nil
}

// PauseJob pauses a job by ID prefix.
func (s *Scheduler) PauseJob(idPrefix, plat, chatID string) (*Job, error) {
	s.mu.Lock()
	j, err := s.findByPrefix(idPrefix, plat, chatID)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if err := s.pauseJobLocked(j); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	save, perr := s.persistJobsLocked()
	s.mu.Unlock()

	if perr != nil {
		return nil, perr
	}
	save()
	return j, nil
}

// ResumeJob resumes a paused job by ID prefix.
func (s *Scheduler) ResumeJob(idPrefix, plat, chatID string) (*Job, error) {
	s.mu.Lock()
	j, err := s.findByPrefix(idPrefix, plat, chatID)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if err := s.resumeJobLocked(j); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	save, perr := s.persistJobsLocked()
	s.mu.Unlock()

	if perr != nil {
		return nil, perr
	}
	save()
	return j, nil
}

// NextRun returns the next scheduled run time for a job.
func (s *Scheduler) NextRun(j *Job) time.Time {
	s.mu.RLock()
	entryID := j.entryID
	s.mu.RUnlock()
	if entryID == 0 {
		return time.Time{}
	}
	entry := s.cron.Entry(entryID)
	return entry.Next
}

// TriggerNow manually executes a job by ID in a new goroutine (for debugging/dashboard).
// Returns an error if the job is not found, paused, or has no prompt.
func (s *Scheduler) TriggerNow(id string) error {
	s.mu.Lock()
	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	}
	if j.Paused {
		s.mu.Unlock()
		return fmt.Errorf("%w: id %q", ErrJobPaused, id)
	}
	if j.Prompt == "" {
		s.mu.Unlock()
		return fmt.Errorf("%w: id %q", ErrJobNoPrompt, id)
	}
	entryID := j.entryID
	// Register the trigger goroutine with triggerWG before releasing s.mu.
	// This prevents a Stop() on another goroutine from observing triggerWG as
	// empty and returning before our goroutine starts. We pair Add(1) here
	// with a Done() in each goroutine body below; if we bail out before
	// spawning (concurrent delete), we Done() the counter inline.
	s.triggerWG.Add(1)
	s.mu.Unlock()

	if entryID != 0 {
		// TriggerNow 不再通过 cron chain 的 WrappedJob.Run()——因为我们要跳过
		// jitter（用户显式 "run now" 期望立刻跑）。改为直接 executeOpt(..., true)。
		// 去 chain 后失去的保护：
		//   1) SkipIfStillRunning —— executeOpt 内部的 jobRunningGuard CAS
		//      同样拒绝重叠，等效覆盖。
		//   2) Recover（panic） —— execute 自身走 session.Send，session 层
		//      panic 已经被上层 recover；即便有残留 panic 也只影响此 goroutine，
		//      不会污染 robfig/cron 调度器。
		// 但必须保留"entry 已被并发 DeleteJob 清掉"的分支：此时 cron.Entry()
		// 的 WrappedJob 为 nil，我们应该把这当作"entry gone"静默退出，不再
		// 走 executeOpt（可能引用已被清理的 session router / job 指针）。
		// 相关测试：TestTriggerNow_EntryGoneReleasesWG（trigger_now_wg_done_test.go）。
		// R192-CRON-B: cron-v2-polish §3.2 jitter。
		entry := s.cron.Entry(entryID)
		if entry.WrappedJob == nil {
			go func() {
				defer s.triggerWG.Done()
				slog.Debug("TriggerNow: cron entry gone (concurrent delete?)", "id", id, "entry_id", entryID)
			}()
		} else {
			jobID := j.ID
			go func() {
				defer s.triggerWG.Done()
				s.mu.RLock()
				cur, ok := s.jobs[jobID]
				paused := ok && cur.Paused
				s.mu.RUnlock()
				if !ok {
					slog.Debug("TriggerNow: job deleted before execute, skipping", "id", jobID)
					return
				}
				if paused {
					slog.Debug("TriggerNow: job paused concurrently, skipping", "id", jobID)
					return
				}
				s.executeOpt(cur, true)
			}()
		}
	} else {
		// Resolve the job by ID inside the goroutine so the freshest pointer
		// is used (matches the cron-tick path in registerJob). If the job was
		// concurrently deleted, skip execution — recordResult would then write
		// to an orphan pointer whose updates are not visible in the snapshot.
		jobID := j.ID
		go func() {
			defer s.triggerWG.Done()
			s.mu.RLock()
			cur, ok := s.jobs[jobID]
			paused := ok && cur.Paused
			s.mu.RUnlock()
			if !ok {
				slog.Debug("TriggerNow: job deleted before execute, skipping", "id", jobID)
				return
			}
			// Honor a Pause that landed between the TriggerNow snapshot and the
			// goroutine starting: the operator's "stop now" intent outranks the
			// in-flight trigger click.
			if paused {
				slog.Debug("TriggerNow: job paused concurrently, skipping", "id", jobID)
				return
			}
			s.executeOpt(cur, true)
		}()
	}
	return nil
}

// registerJob registers a job with the robfig/cron scheduler.
//
// The closure captures the job's ID rather than the *Job pointer: if the
// job is removed and re-added (UpdateJob path) while the scheduler goroutine
// holds an old entry, we want the next tick to resolve the currently-registered
// job rather than fire against a stale pointer whose fields may have diverged
// from the user's intent.
func (s *Scheduler) registerJob(j *Job) error {
	jobID := j.ID
	entryID, err := s.cron.AddFunc(j.Schedule, func() {
		s.mu.RLock()
		cur, ok := s.jobs[jobID]
		paused := ok && cur.Paused
		s.mu.RUnlock()
		if !ok {
			slog.Debug("cron: scheduled job no longer registered, skipping", "id", jobID)
			return
		}
		// A Pause that lands between cron-tick dispatch and our re-lock should
		// be honored; otherwise the user sees a paused job still firing once.
		// PauseJobByID removes the entry via cron.Remove(), so normally this
		// tick wouldn't fire — but robfig/cron may already be mid-dispatch when
		// Remove runs, yielding exactly this race.
		if paused {
			slog.Debug("cron: tick fired for job paused concurrently, skipping", "id", jobID)
			return
		}
		s.executeOpt(cur, false)
	})
	if err != nil {
		return fmt.Errorf("register cron: %w", err)
	}
	j.entryID = entryID
	return nil
}

// jobInflight returns a lazily created *runInflight per job ID. The
// embedded atomic.Bool keeps the original CAS-gate semantics (used by
// executeOpt to reject concurrent runs); the surrounding metadata fields
// expose RunID/StartedAt/Phase to the list API for the cron-run-history
// P0 visibility work.
//
// Entries are intentionally NOT cleared on DeleteJob — see runningJobs's
// struct comment for the ID-reuse split-CAS rationale.
func (s *Scheduler) jobInflight(id string) *runInflight {
	if v, ok := s.runningJobs.Load(id); ok {
		if inf, ok := v.(*runInflight); ok && inf != nil {
			return inf
		}
	}
	guard := &runInflight{}
	actual, _ := s.runningJobs.LoadOrStore(id, guard)
	if inf, ok := actual.(*runInflight); ok && inf != nil {
		return inf
	}
	// Should be unreachable given LoadOrStore's contract, but never return
	// nil to callers — they immediately call methods on the result.
	return guard
}

// jobSnapshot captures the mutable Job fields executeOpt reads under s.mu so
// the long-running send/notify pipeline can run without holding the lock.
// Snapshot is taken once after the rate-limit/jitter gate and reused for the
// rest of the execution; concurrent SetJobPrompt/UpdateJob therefore land
// for the next tick rather than racing the in-flight result. The shape
// mirrors the original inline reads — no fields added/removed.
type jobSnapshot struct {
	prompt     string
	workDir    string
	jobID      string
	platName   string
	chatID     string
	notifyPlat string
	notifyChat string
	notify     *bool // nil = unset
	fresh      bool
	schedule   string
	backend    string // "" = router default
}

// snapshotJob reads j under s.mu so a concurrent SetJobPrompt /
// UpdateJob cannot tear the read across fields. Always returns a value
// (never nil); j is dereferenced inside the lock. RLock is sufficient
// since snapshotJob is read-only and runs from executeOpt outside s.mu.
//
// LOCK: Must NOT be called while s.mu is already held — acquires
// s.mu.RLock internally. robfig/cron callbacks must never hold s.mu
// when invoking snapshotJob (R227-CR-3).
func (s *Scheduler) snapshotJob(j *Job) jobSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := jobSnapshot{
		prompt:     j.Prompt,
		workDir:    j.WorkDir,
		jobID:      j.ID,
		platName:   j.Platform,
		chatID:     j.ChatID,
		notifyPlat: j.NotifyPlatform,
		notifyChat: j.NotifyChatID,
		fresh:      j.FreshContext,
		schedule:   j.Schedule,
		backend:    j.Backend,
	}
	if j.Notify != nil {
		v := *j.Notify
		snap.notify = &v
	}
	return snap
}

// preflightResult bundles the closure / continuation-flag returned by the
// P0 preflight wrapper. Pulled out so executeOpt's call site reads as one
// destructure instead of two return values mixed with a side-effect rec.
type preflightResult struct {
	stubRefresh func()
}

// preflightArgs bundles the inputs to freshContextPreflightP0. R229-CR-8.
// Mirrors finishArgs's struct-bag pattern: the helper has 8 inputs that all
// flow through to the same finishRun/deliverNotice call sites and keeping
// them as positional args made future additions (e.g. a new error-class)
// risk silent argument-order swaps. Named fields also let tests express
// intent without reading parameter positions.
type preflightArgs struct {
	job       *Job
	snap      jobSnapshot
	key       string
	lg        *slog.Logger
	notifyTo  NotifyTarget
	runID     string
	startedAt time.Time
	trigger   TriggerKind
}

// freshContextPreflightP0 handles the fresh-mode prologue: ctx-cancel guard
// (CRON3), work-dir reachability check (CRON2), Reset, and the post-Reset
// existence re-check that prevents a leaked CLI process tied to a deleted
// job ("cron:<id>" orphan). Each failure branch records a (RunState,
// ErrorClass) tuple via finishRun so the run-history terminal protocol
// (broadcast cron_run_ended + counters + LastErrorClass write) participates.
//
// Returns:
//   - preflightResult.stubRefresh: closure that re-registers the sidebar
//     stub on error paths so the cron row stays visible. Caller invokes
//     after error branches; never invoke on success (live session owns
//     the row).
//   - ok: false means the caller MUST return immediately. The helper has
//     already written the appropriate slog.Info/Warn + finishRun() for
//     the failure mode.
//
// In persistent mode (snap.fresh=false) the helper short-circuits with
// ok=true and a no-op stubRefresh so the caller's flow is uniform.
func (s *Scheduler) freshContextPreflightP0(args preflightArgs) (preflightResult, bool) {
	snap := args.snap
	lg := args.lg
	if !snap.fresh {
		return preflightResult{stubRefresh: func() {}}, true
	}
	if err := s.stopCtx.Err(); err != nil {
		lg.Info("cron fresh spawn suppressed during shutdown", "err", err)
		// Treat shutdown-cancel as canceled (not failed); skipPersist=true
		// preserves prior recordResult semantics where ctx-cancel did not
		// touch LastRunAt. The broadcast still emits so the dashboard sees
		// the run's terminal frame.
		s.finishRun(finishArgs{
			job: args.job, runID: args.runID, startedAt: args.startedAt, trigger: args.trigger,
			state: RunStateCanceled, errClass: ErrClassCanceled, errMsg: err.Error(),
			skipPersist: true,
			prompt:      snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
		})
		return preflightResult{stubRefresh: func() {}}, false
	}
	if !workDirReachable(snap.workDir) {
		lg.Warn("cron fresh spawn aborted: work_dir unreachable",
			"work_dir", snap.workDir)
		s.finishRun(finishArgs{
			job: args.job, runID: args.runID, startedAt: args.startedAt, trigger: args.trigger,
			state: RunStateFailed, errClass: ErrClassWorkDirUnreachable,
			errMsg: "work_dir unreachable",
			prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
		})
		s.deliverNotice(args.notifyTo, fmt.Sprintf("[Cron %s] 工作目录不可达，本次执行已跳过。", snap.jobID))
		return preflightResult{stubRefresh: func() {}}, false
	}
	s.router.Reset(args.key)
	lg.Info("cron fresh context: session reset before run")
	stubRefresh := func() {
		s.mu.RLock()
		jobCopy, exists := s.jobs[snap.jobID]
		var j2 Job
		if exists {
			j2 = *jobCopy
		}
		s.mu.RUnlock()
		if exists {
			s.registerStub(&j2)
		}
	}
	s.mu.RLock()
	_, stillExists := s.jobs[snap.jobID]
	s.mu.RUnlock()
	if !stillExists {
		lg.Info("cron job deleted mid-execute, skipping GetOrCreate")
		// Job deleted mid-execute: treat as canceled; no recordResult
		// (matches historical behaviour) but broadcast for visibility.
		s.finishRun(finishArgs{
			job: args.job, runID: args.runID, startedAt: args.startedAt, trigger: args.trigger,
			state: RunStateCanceled, errClass: ErrClassCanceled,
			errMsg: "job deleted mid-execute", skipPersist: true,
			prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
		})
		return preflightResult{stubRefresh: stubRefresh}, false
	}
	return preflightResult{stubRefresh: stubRefresh}, true
}

// executeOpt runs a cron job: send prompt to session, post result to chat.
// viaTriggerNow=true skips jitter delay (explicit user "run now" expects
// immediate execution); scheduled tick callers pass false.
//
// P0 cron-run-history (RFC §5):
//  1. CAS gate populates *runInflight with RunID/StartedAt/Trigger/Phase.
//  2. WS broadcast `cron_run_started` after CAS, before notify-target resolve.
//  3. Each error branch maps to a specific (RunState, ErrorClass) tuple via
//     finishRun, which:
//     - writes recordResult (LastResult/LastError/LastErrorClass + Counters)
//     - emits cron_run_ended broadcast
//     - bumps the per-state metrics.CronRun*Total counter
//     so all terminal paths share one observability hook.
//
// deadlineInterrupter is the narrow capability runDeadlineWatchdog needs
// from a session: a way to abort an in-flight CLI turn via the protocol's
// control_request channel. *session.ManagedSession satisfies this; cron
// tests stub it with a counting mock to assert the watchdog fired
// exactly when the deadline elapsed.
type deadlineInterrupter interface {
	InterruptViaControl() session.InterruptOutcome
}

// abortResult bundles the watchdog's exit signal: whether it actually
// fired the interrupt (i.e. the ctx ended via DeadlineExceeded, not via
// success-path Cancel) and what the InterruptViaControl outcome was when
// it did. The fired flag is the discriminator the caller logs.
type abortResult struct {
	outcome session.InterruptOutcome
	fired   bool
}

// runDeadlineWatchdog spawns a goroutine that waits on ctx and fires
// sess.InterruptViaControl exactly when ctx ends with DeadlineExceeded.
// The watchdog must run concurrently with sess.Send, NOT after — Send's
// internal defer flips Process.State Running→Ready the instant ctx fires,
// and InterruptViaControl gates on State==StateRunning, so calling it
// post-Send is dead code (returns ErrNoActiveTurn → outcome=no_turn).
//
// The returned channel emits exactly one abortResult and is closed
// implicitly when read. Caller must drain it before returning so the
// goroutine cannot outlive the cron run (otherwise a fast cron tick could
// race the next session.Reset against the in-flight interrupt write).
//
// On the success / non-deadline error path the caller cancels ctx
// explicitly; the watchdog observes ctx.Err()==Canceled, skips
// InterruptViaControl, and returns abortResult{fired:false}.
func runDeadlineWatchdog(ctx context.Context, sess deadlineInterrupter) <-chan abortResult {
	ch := make(chan abortResult, 1)
	go func() {
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			ch <- abortResult{outcome: sess.InterruptViaControl(), fired: true}
			return
		}
		ch <- abortResult{}
	}()
	return ch
}

func (s *Scheduler) executeOpt(j *Job, viaTriggerNow bool) {
	// Guard against concurrent execution of the same job. The cron chain's
	// SkipIfStillRunning protects the scheduled-tick path, but TriggerNow
	// that arrives while a tick is in flight bypasses the chain entirely
	// (it calls execute directly when entryID == 0 or Run() on the entry
	// which is separately serialized). The per-job *runInflight (containing
	// the CAS atomic.Bool) keeps a uniform CAS gate while exposing run
	// metadata to the list API.
	inflight := s.jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		slog.Info("cron: job already running, skipping overlap", "cron_id", j.ID)
		// Overlap is a skipped state (no LastRunAt update). Counters /
		// broadcast still fire so dashboards can surface the skip.
		s.emitOverlapSkipped(j, viaTriggerNow)
		return
	}
	defer func() {
		inflight.running.Store(false)
		inflight.reset()
		metrics.CronRunInflight.Add(-1)
	}()

	// Populate the inflight metadata under the CAS-true window. RunID is
	// generated once per run; StartedAt is captured before jitter so the
	// "running 12s" badge in the UI counts true wall-clock from CAS.
	runID := generateRunID()
	startedAt := time.Now()
	trigger := TriggerScheduled
	if viaTriggerNow {
		trigger = TriggerManual
	}
	{
		rid := runID
		st := startedAt
		ph := PhaseQueued
		tr := string(trigger)
		empty := ""
		inflight.runID.Store(&rid)
		inflight.startedAt.Store(&st)
		inflight.phase.Store(&ph)
		inflight.trigger.Store(&tr)
		inflight.sessionID.Store(&empty)
		inflight.freshSnap.Store(j.FreshContext)
	}
	metrics.CronRunInflight.Add(1)
	metrics.CronRunStartedTotal.Add(1)

	// Apply jitter after CAS, before snapshot. After-CAS so concurrent overlap
	// triggers are rejected immediately. Before-snapshot so an UpdateJob that
	// lands during the jitter window still lets the subsequent snapshot read
	// the new Prompt / WorkDir (matches the "edits take effect immediately"
	// operator expectation). TriggerNow skips jitter to preserve the
	// "run now = run now" semantics.
	if !viaTriggerNow && s.jitterMax > 0 {
		inflight.setPhase(PhaseJittering)
		applyJitter(s.stopCtx, j.Schedule, s.jitterMax)

		// R220-GO-3: a DeleteJob that lands during the jitter window leaves
		// the inflight CAS still held until we finish — blocking TriggerNow
		// for the same id with an "already running" overlap skip. Re-check
		// the job is still registered after the jitter wait so the deferred
		// inflight.running.Store(false) above releases promptly. snapshotJob
		// reads under s.mu so a stale dereference is impossible after Delete
		// (the field reads return the last-known values and we never use
		// them past this point).
		s.mu.RLock()
		_, stillRegistered := s.jobs[j.ID]
		s.mu.RUnlock()
		if !stillRegistered {
			slog.Debug("cron: job deleted during jitter window, aborting run",
				"cron_id", j.ID, "run_id", runID)
			return
		}
	}

	// Snapshot mutable Job fields once under s.mu so the rest of the
	// execution can run lock-free; concurrent SetJobPrompt/UpdateJob land
	// for the next tick rather than racing this in-flight result.
	snap := s.snapshotJob(j)
	inflight.freshSnap.Store(snap.fresh)

	// Resolve the effective notification target. Returns empty struct
	// when no delivery should happen, so both success and failure paths
	// below can call notify*() unconditionally-guarded by IsSet().
	notifyTo := s.resolveNotifyTarget(snap.platName, snap.chatID, snap.notifyPlat, snap.notifyChat, snap.notify)

	// Broadcast started — placed after snapshot so the event carries the
	// effective fresh flag and after notifyTo resolution so server-side
	// hub locks aren't held while we read s.mu.
	s.emitRunStarted(RunStartedEvent{
		JobID:     snap.jobID,
		RunID:     runID,
		StartedAt: startedAt,
		Trigger:   trigger,
		Fresh:     snap.fresh,
	})

	// `lg` instead of `log` to avoid shadowing the standard `log` package
	// imported at the top of the file (R60-GO-M2).
	lg := slog.With("cron_id", snap.jobID, "platform", snap.platName, "chat", snap.chatID, "run_id", runID)
	lg.Info("cron job executing", "prompt_len", len(snap.prompt))

	// Per-job timeout is always s.execTimeout (period scaling was removed —
	// see computeJobTimeout's godoc for why robfig/cron's SkipIfStillRunning
	// chain wrapper handles long-running tasks correctly).
	jobTimeout := computeJobTimeout(snap.schedule, s.execTimeout)
	ctx, cancel := context.WithTimeout(s.stopCtx, jobTimeout)
	defer cancel()

	// s.agentCommands and s.agents are assigned once at scheduler
	// construction (cfg.AgentCommands / cfg.Agents) and never mutated;
	// reading them without s.mu is safe. If a future SetAgents API is
	// introduced both reads must move under s.mu.
	agentID, cleanText := session.ResolveAgent(snap.prompt, s.agentCommands)
	opts := s.agents[agentID]
	// R228-GO-P3-8: clip ExtraArgs to its own length so any subsequent append
	// downstream allocates a fresh backing array instead of mutating the
	// shared map value. Mirrors session/routing.go's slices.Clone defence.
	if len(opts.ExtraArgs) > 0 {
		opts.ExtraArgs = opts.ExtraArgs[:len(opts.ExtraArgs):len(opts.ExtraArgs)]
	}
	opts.Exempt = true // cron sessions must not count toward maxProcs or evict user sessions
	// Sprint 6c (docs/rfc/multi-backend.md §9): per-job backend override.
	// Empty snap.backend leaves opts.Backend untouched ("" already routes
	// through the router default, and the agent profile may have its own
	// backend pinned). A non-empty value wins because the user explicitly
	// picked it for this cron job from the dashboard. validateBackend at
	// the router boundary still rejects shape-invalid input (control chars,
	// overlength); unknown-but-well-formed backends fall back via wrapperFor.
	if snap.backend != "" {
		opts.Backend = snap.backend
	}
	if snap.workDir != "" {
		// Re-check allowedRoot at execute time to close the symlink-swap
		// race: validateWorkspace at creation resolved symlinks once, but
		// the target could have been retargeted since.
		if s.allowedRoot != "" && !workDirUnderRoot(snap.workDir, s.allowedRoot, s.allowedRootResolved) {
			lg.Warn("cron job work_dir outside allowed_root; aborting run",
				"work_dir", snap.workDir)
			s.finishRun(finishArgs{
				job: j, runID: runID, startedAt: startedAt, trigger: trigger,
				state: RunStateFailed, errClass: ErrClassWorkDirOutsideRoot,
				errMsg: "work_dir outside allowed_root",
				prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
			})
			return
		}
		opts.Workspace = filepath.Clean(snap.workDir)
	}
	key := session.CronKey(snap.jobID)

	// Fresh mode: drop any existing session (and its process + history) so
	// GetOrCreate spawns a brand-new CLI. The helper handles ctx-cancel,
	// workDir reachability, and post-Reset job-existence re-check. On
	// error paths the returned stubRefresh re-registers the sidebar row
	// so the cron entry doesn't vanish from the dashboard. On the success
	// path we skip stubRefresh because the live session carries its own
	// sidebar entry. Persistent mode short-circuits inside the helper
	// with a no-op stubRefresh.
	preflight, ok := s.freshContextPreflightP0(preflightArgs{
		job: j, snap: snap, key: key, lg: lg, notifyTo: notifyTo,
		runID: runID, startedAt: startedAt, trigger: trigger,
	})
	if !ok {
		preflight.stubRefresh()
		return
	}

	inflight.setPhase(PhaseSpawning)
	sess, _, err := s.router.GetOrCreate(ctx, key, opts)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Parent ctx cancelled mid-flight (graceful shutdown or job
			// deletion overlapping execute). The job will either be re-run
			// on the next tick or is intentionally gone; either way an IM
			// notification would be spam and the stored LastError would
			// falsely blame the job itself.
			lg.Info("cron session cancelled", "err", err)
			s.finishRun(finishArgs{
				job: j, runID: runID, startedAt: startedAt, trigger: trigger,
				state: RunStateCanceled, errClass: ErrClassCanceled, errMsg: err.Error(),
				skipPersist: true, // 与 historical recordResult skip 一致
				prompt:      snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
			})
			preflight.stubRefresh()
			return
		}
		state := RunStateFailed
		errClass := ErrClassSessionError
		if errors.Is(err, context.DeadlineExceeded) {
			lg.Info("cron session deadline exceeded", "err", err)
			state = RunStateTimedOut
			errClass = ErrClassDeadlineExceeded
		} else {
			lg.Error("cron session error", "err", err)
		}
		s.finishRun(finishArgs{
			job: j, runID: runID, startedAt: startedAt, trigger: trigger,
			state: state, errClass: errClass, errMsg: fmt.Sprintf("session error: %v", err),
			prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
		})
		s.deliverNotice(notifyTo, fmt.Sprintf("[Cron %s] 执行跳过，请稍后重试。", snap.jobID))
		preflight.stubRefresh()
		return
	}

	// R191-ARCH-M5: Send uses a ctx derived from Background, not stopCtx.
	// Rationale: once GetOrCreate has handed us a live session we should
	// either record a result or a real error. If we piggy-back on stopCtx
	// here, Scheduler.Stop()'s first act (stopCancel) cancels this ctx and
	// the in-flight Send's result is silently dropped — the job records no
	// LastRunAt, is re-run on the next start, and "cron send cancelled"
	// logs make shutdown look like a failure. notifyTarget (this file)
	// already uses Background for delivery after shutdown for the same
	// reason; make Send consistent. Shutdown latency is bounded by
	// Router.Shutdown's drain timeout (ShutdownTimeout, 30s in
	// internal/session) + cron.Stop()'s own cron.Stop() chain drain.
	sendCtx, sendCancel := context.WithTimeout(context.Background(), jobTimeout)
	defer sendCancel()
	inflight.setPhase(PhaseSending)

	// Watchdog: deadline-fired interrupt of the in-flight CLI turn. See
	// runDeadlineWatchdog for the rationale (must fire BEFORE Send returns,
	// otherwise Process.State has already flipped to Ready and
	// InterruptViaControl returns ErrNoActiveTurn → no-op).
	abortCh := runDeadlineWatchdog(sendCtx, sess)

	// Direct Send without sendWithBroadcast — cron jobs notify via onExecute callback instead.
	result, err := sess.Send(sendCtx, cleanText, nil, nil)
	// Cancel sendCtx so the watchdog returns promptly on the success / non-
	// deadline error path; on the deadline path it's already done. Block
	// on abortCh so the InterruptViaControl call (if any) completes before
	// we record the run state — otherwise a fast cron tick could overlap
	// the next session.Reset with the in-flight interrupt write.
	sendCancel()
	abort := <-abortCh
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Same rationale as the session-error branch above: suppress
			// the operator-facing notice so shutdown races don't look like
			// real failures. abort.fired can still be true here when a
			// stopCtx cancel races a near-deadline tick — surface it so
			// operators have a signal that an interrupt attempt happened
			// during the cancel path.
			lg.Info("cron send cancelled",
				"err", err,
				"abort_fired", abort.fired,
				"abort_outcome", abort.outcome)
			s.finishRun(finishArgs{
				job: j, runID: runID, startedAt: startedAt, trigger: trigger,
				state: RunStateCanceled, errClass: ErrClassCanceled, errMsg: err.Error(),
				skipPersist: true,
				prompt:      snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
			})
			preflight.stubRefresh()
			return
		}
		state := RunStateFailed
		errClass := ErrClassSendError
		if errors.Is(err, context.DeadlineExceeded) {
			state = RunStateTimedOut
			errClass = ErrClassDeadlineExceeded
			// Log alongside the watchdog outcome so operators see both the
			// deadline AND whether the CLI was successfully interrupted in
			// the same line. ACP backends report "unsupported" here — we
			// accept the silent no-op since ACP cron jobs are rare and a
			// SIGINT fallback would couple two different abort semantics.
			lg.Info("cron send deadline exceeded",
				"err", err,
				"abort_fired", abort.fired,
				"abort_outcome", abort.outcome)
		} else {
			lg.Error("cron send error", "err", err)
		}
		s.finishRun(finishArgs{
			job: j, runID: runID, startedAt: startedAt, trigger: trigger,
			state: state, errClass: errClass, errMsg: fmt.Sprintf("send error: %v", err),
			prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
		})
		s.deliverNotice(notifyTo, fmt.Sprintf("[Cron %s] 执行失败，请稍后重试。", snap.jobID))
		preflight.stubRefresh()
		return
	}
	if result.SessionID != "" {
		inflight.setSessionID(result.SessionID)
	}

	elapsed := time.Since(startedAt)
	lg.Info("cron job completed",
		"result_len", len(result.Text),
		"elapsed_ms", elapsed.Milliseconds())
	if elapsed > cronSlowThreshold {
		// R208-OBS1: poor-man's histogram — a single counter that fires
		// when a successful execution takes longer than cronSlowThreshold.
		// Wired here (not in finishRun) so only success-path latency
		// counts; error paths already surface via metrics state counters.
		metrics.CronExecutionSlowTotal.Add(1)
		lg.Warn("cron execution slow",
			"job_id", snap.jobID,
			"elapsed_ms", elapsed.Milliseconds(),
			"threshold_ms", cronSlowThreshold.Milliseconds())
	}
	// 把本次产生的 Claude session_id 也记下来：fresh_context=true 的
	// 路径下一次 Reset 会清掉 stub 的 chain，不保留这个 ID 的话
	// dashboard 点击 cron 侧边栏就看不到上一次的 JSONL 历史。
	// Send 路径的 result 帧总会带 SessionID（process.go 成功分支会填），
	// 传空只会出现在错误路径，finishRun 的 "" 分支自行短路。
	s.finishRun(finishArgs{
		job: j, runID: runID, startedAt: startedAt, trigger: trigger,
		state: RunStateSucceeded, sessionID: result.SessionID, result: result.Text,
		prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
	})

	replyText := fmt.Sprintf("[Cron %s] %s", snap.jobID, result.Text)
	s.deliverNotice(notifyTo, replyText)
}

// finishArgs bundles the parameters of finishRun so each call site reads
// as a struct literal — many fields are optional (errClass / errMsg / sessionID
// / result / skipPersist) and a positional signature would be brittle.
//
// snapshot fields (prompt/workDir/fresh) are populated only on paths that
// have already taken the snapshotJob() — overlapSkipped / pre-snapshot
// preflight failures pass them as zero values, which CronRun renders as
// empty (the dashboard will fall back to Job.Prompt for display).
type finishArgs struct {
	job       *Job
	runID     string
	startedAt time.Time
	trigger   TriggerKind
	state     RunState
	sessionID string
	result    string
	errClass  ErrorClass
	errMsg    string
	// skipPersist 同时控制两件事：跳过 Job 字段更新（LastRunAt/LastResult/
	// LastError/LastErrorClass/Counters）和跳过 CronRun 磁盘历史。当前所有
	// 调用点这两件事都同步：canceled / overlap_skipped / job-deleted-mid-
	// execute 三种 transient 终态 — 都不应该污染 Job 快照，也不应该塞进
	// runs/<jobID>/。如果将来要独立控制（比如"想记历史但不更新 Counters"），
	// 拆成 skipJobUpdate / skipHistoryRecord 两个 bool；当前合一是 RFC §5
	// 状态机表的直接映射。Metrics + WS broadcast 不受 skipPersist 影响——
	// 故意如此，dashboard 必须能看到 skipped/canceled 帧。R220-ARCH-1.
	skipPersist bool
	prompt      string
	workDir     string
	fresh       bool
}

// finishRun is the single terminal hook for every cron execution path.
// It centralises:
//   - per-state metrics increment (CronRun*Total)
//   - persistent state write via recordResult (success / non-canceled error)
//   - cron_run_ended WS broadcast
//   - JobRunCounters bump (under s.mu, alongside recordResult)
//
// Centralising avoids the historical pattern of recordResult-and-deliver-and-
// log scattered across executeOpt's seven branches; adding a new error class
// is now one mapping plus one finishArgs literal at the call site.
func (s *Scheduler) finishRun(a finishArgs) {
	endedAt := time.Now()
	durationMS := endedAt.Sub(a.startedAt).Milliseconds()
	if durationMS < 0 {
		durationMS = 0 // monotonic clock skew safety
	}
	s.bumpRunStateMetrics(a.state)

	// Persist (LastRunAt/LastResult/LastError/Counters) for terminal paths
	// that historically updated state. Canceled / shutdown paths skipPersist
	// to preserve "next start retries" semantics; same paths also skip the
	// CronRun history record (transient by definition; would inflate runs/
	// with shutdown noise).
	//
	// SECURITY: persistedResult / persistedErrMsg are post-redact + post-
	// sanitise strings. Both the on-disk CronRun and the WS broadcast must
	// use these — never the raw a.result / a.errMsg — otherwise an error
	// containing an absolute filesystem path (e.g. "session error: open
	// /home/ops/private-repo: permission denied") leaks the workspace
	// layout to every authenticated dashboard client. R220-SEC-1.
	//
	// On the skipPersist path recordResultP0WithSanitised is bypassed, so
	// we apply the same redact + sanitise pipeline inline. Cheap (regex-
	// free path scan + ASCII control filter) and ensures no broadcast
	// branch can echo raw err.Error() / fmt.Sprintf output to clients.
	//
	// jobPersistOK 表示 Job 字段 + cron_jobs.json 落盘是否真的成功。
	// false → marshal 失败回滚了 Job in-memory 字段，或者 Job 已被并发
	// 删除。两种情况下都不该再写 CronRun history（dashboard list 读
	// Job 字段，timeline 读 CronRun，二者必须同步可见或同步缺失）。
	// 这是 R220-ARCH-2 一致性窗口的修复。
	persistedResult := a.result
	persistedErrMsg := a.errMsg
	jobPersistOK := false
	if !a.skipPersist {
		persistedResult, persistedErrMsg, jobPersistOK = s.recordResultP0WithSanitised(a.job, a.result, a.errMsg, a.sessionID, a.errClass, a.state)
	} else {
		persistedResult = sanitiseRunResult(persistedResult)
		persistedErrMsg = sanitiseRunErrMsg(persistedErrMsg)
	}

	// CronRun history (P1). Conditions:
	//   - skipPersist=false（这次 run 应该被记录）
	//   - jobPersistOK=true（Job 端写盘成功；否则 disk-divergence 风险）
	//   - runStore 启用
	if !a.skipPersist && jobPersistOK && s.runStore != nil {
		s.runStore.Append(&CronRun{
			RunID:       a.runID,
			JobID:       a.job.ID,
			State:       a.state,
			Trigger:     a.trigger,
			StartedAt:   a.startedAt,
			EndedAt:     endedAt,
			DurationMS:  durationMS,
			SessionID:   a.sessionID,
			Prompt:      a.prompt,
			WorkDir:     a.workDir,
			Fresh:       a.fresh,
			Result:      persistedResult,
			ResultBytes: len(a.result),
			ErrorClass:  a.errClass,
			ErrorMsg:    persistedErrMsg,
		})
	}

	// Broadcast last so server-side hub locks aren't held while we hold s.mu.
	// ErrorMsg uses persistedErrMsg (post-redact, post-sanitise) — see the
	// SECURITY note above for why a.errMsg is never used here.
	s.emitRunEnded(RunEndedEvent{
		JobID:      a.job.ID,
		RunID:      a.runID,
		State:      a.state,
		StartedAt:  a.startedAt,
		EndedAt:    endedAt,
		DurationMS: durationMS,
		SessionID:  a.sessionID,
		ErrorClass: a.errClass,
		ErrorMsg:   persistedErrMsg,
		Trigger:    a.trigger,
	})
	metrics.CronRunEndedTotal.Add(1)
}

// sanitiseRunResult applies the same rune truncation + SanitizeForLog
// pipeline that recordResultP0WithSanitised uses, factored out so the
// skipPersist path of finishRun can reach the same byte-output without
// touching s.mu / persistJobsLocked. Idempotent w.r.t. clean strings.
func sanitiseRunResult(s string) string {
	if trimmed := textutil.TruncateRunesNoEllipsis(s, maxStoredResultRunes); len(trimmed) < len(s) {
		s = trimmed + truncatedSuffix
	}
	// SanitizeForLog's maxLen is byte-counted, so extend the cap by the
	// suffix length so a 4K-rune input that just got the "…[truncated]"
	// marker appended doesn't have its suffix byte-clipped on the way
	// out. R232-PERF-9.
	return osutil.SanitizeForLog(s, maxStoredResultRunes+len(truncatedSuffix))
}

// truncatedSuffix marks where sanitiseRunResult / recordResultP0WithSanitised
// cut a result that exceeded maxStoredResultRunes. Centralised so the
// downstream SanitizeForLog cap can compensate for its byte length.
const truncatedSuffix = "…[truncated]"

// sanitiseRunErrMsg applies the cron error-redaction + log-injection
// scrub used by recordResultP0WithSanitised, for skipPersist branches
// (canceled / shutdown / overlap-skipped) whose error strings still
// flow into WS broadcasts and must not leak filesystem paths.
func sanitiseRunErrMsg(s string) string {
	s = redactPathsInCronError(s)
	return osutil.SanitizeForLog(s, 512)
}

// bumpRunStateMetrics increments the per-state counter for the terminal
// transition. Mirrored in metrics.go and pinned by counter_wiring_contract_test.
func (s *Scheduler) bumpRunStateMetrics(state RunState) {
	switch state {
	case RunStateSucceeded:
		metrics.CronRunSucceededTotal.Add(1)
	case RunStateFailed:
		metrics.CronRunFailedTotal.Add(1)
	case RunStateSkipped:
		metrics.CronRunSkippedTotal.Add(1)
	case RunStateTimedOut:
		metrics.CronRunTimedOutTotal.Add(1)
	case RunStateCanceled:
		metrics.CronRunCanceledTotal.Add(1)
	}
}

// emitOverlapSkipped is the synthetic terminal event for the CAS-rejected
// path. The CAS gate trips before any inflight metadata is populated, so
// we synthesise a RunID + StartedAt locally and emit started→ended back-to-
// back so the dashboard timeline still shows the skip. State counter +
// ended counter both bump.
func (s *Scheduler) emitOverlapSkipped(j *Job, viaTriggerNow bool) {
	runID := generateRunID()
	startedAt := time.Now()
	trigger := TriggerScheduled
	if viaTriggerNow {
		trigger = TriggerManual
	}
	s.emitRunStarted(RunStartedEvent{
		JobID:     j.ID,
		RunID:     runID,
		StartedAt: startedAt,
		Trigger:   trigger,
	})
	metrics.CronRunStartedTotal.Add(1)
	s.finishRun(finishArgs{
		job: j, runID: runID, startedAt: startedAt, trigger: trigger,
		state: RunStateSkipped, errClass: ErrClassOverlapSkipped,
		errMsg: "previous run still in flight", skipPersist: true,
	})
}

// emitRunStarted invokes the registered server-side hook outside s.mu so
// hub locks may be acquired by the handler without inversion risk. nil
// hook = no broadcast (used by tests / no-WS deployments).
func (s *Scheduler) emitRunStarted(ev RunStartedEvent) {
	if fn := s.onRunStarted.Load(); fn != nil {
		(*fn)(ev)
	}
}

func (s *Scheduler) emitRunEnded(ev RunEndedEvent) {
	if fn := s.onRunEnded.Load(); fn != nil {
		(*fn)(ev)
	}
}

// recordResultP0WithSanitised persists the terminal result (LastResult /
// LastError / LastErrorClass / Counters) for non-skipPersist paths and
// returns the post-sanitised (result, errMsg) pair so finishRun can reuse
// the same byte content in the CronRun history record. The two outputs
// must remain byte-identical or the dashboard list would diverge from
// runs/<jobID>/<run_id>.json on disk.
//
// Returns ok=false in two failure modes:
//   - target Job has been deleted between snapshot and recordResult (race
//     with DeleteJobByID): caller should also skip the CronRun history
//     record because writing it would create a runs/<jobID>/ subtree for
//     a job that no longer exists in s.jobs.
//   - persistJobsLocked / marshal failed and we rolled back Job fields
//     in-memory: caller MUST also skip the CronRun history record so
//     dashboard list view (reads Job fields) and timeline view (reads
//     CronRun) don't diverge — they'd otherwise show contradictory state
//     for the same run. R220-ARCH-2.
//
// R220-GO-1: previously a thin recordResultP0 wrapper existed for tests
// pinning the (j, result, errMsg, sessionID, errClass, state) signature.
// No production caller used it; finishRun goes direct. The wrapper was
// dead code and has been removed; tests assert on outcomes (Job fields,
// CronRun summary), not wrapper presence.
func (s *Scheduler) recordResultP0WithSanitised(j *Job, result, errMsg, sessionID string, errClass ErrorClass, state RunState) (string, string, bool) {
	if trimmed := textutil.TruncateRunesNoEllipsis(result, maxStoredResultRunes); len(trimmed) < len(result) {
		result = trimmed + truncatedSuffix
	}
	errMsg = redactPathsInCronError(errMsg)
	// Extend SanitizeForLog's byte cap by the suffix length so an
	// already-truncated result keeps the trailing marker intact;
	// otherwise byte-level truncation could clip mid-suffix.
	// R232-PERF-9.
	result = osutil.SanitizeForLog(result, maxStoredResultRunes+len(truncatedSuffix))
	errMsg = osutil.SanitizeForLog(errMsg, 512)

	s.mu.Lock()
	if _, ok := s.jobs[j.ID]; !ok {
		s.mu.Unlock()
		return result, errMsg, false
	}
	prev := struct {
		LastRunAt      time.Time
		LastResult     string
		LastError      string
		LastErrorClass ErrorClass
		LastSessionID  string
		Counters       JobRunCounters
	}{j.LastRunAt, j.LastResult, j.LastError, j.LastErrorClass, j.LastSessionID, j.RunCounters}

	j.LastRunAt = time.Now()
	j.LastResult = result
	j.LastError = errMsg
	j.LastErrorClass = errClass
	if sessionID != "" {
		j.LastSessionID = sessionID
	}
	j.RunCounters.addRun(state)

	save, perr := s.persistJobsLocked()
	if perr != nil {
		j.LastRunAt = prev.LastRunAt
		j.LastResult = prev.LastResult
		j.LastError = prev.LastError
		j.LastErrorClass = prev.LastErrorClass
		j.LastSessionID = prev.LastSessionID
		j.RunCounters = prev.Counters
		s.mu.Unlock()
		slog.Warn("cron: recordResultP0 persist failed; in-memory result reverted",
			"job_id", j.ID, "err", perr)
		return result, errMsg, false
	}
	s.mu.Unlock()

	save()
	if fn := s.onExecute.Load(); fn != nil {
		(*fn)(j.ID, result, errMsg)
	}
	return result, errMsg, true
}

// resolveNotifyTarget picks the IM destination for this execution's
// completion notice. Priority:
//  1. Per-job NotifyPlatform/NotifyChatID (always honored when both set).
//  2. notify==true + scheduler default target.
//  3. notify==false disables delivery even for IM-created jobs.
//  4. notify==nil (unset) preserves legacy behavior: IM-created jobs reply
//     to their own source chat; dashboard-created jobs stay silent.
func (s *Scheduler) resolveNotifyTarget(platName, chatID, notifyPlat, notifyChat string, notify *bool) NotifyTarget {
	// Explicit disable wins over everything.
	if notify != nil && !*notify {
		return NotifyTarget{}
	}

	// Per-job override always wins when fully specified.
	if notifyPlat != "" && notifyChat != "" {
		return NotifyTarget{Platform: notifyPlat, ChatID: notifyChat}
	}

	// Explicit enable: fall back to scheduler default.
	if notify != nil && *notify {
		if s.notifyDefault.IsSet() {
			return s.notifyDefault
		}
		// Enabled but no target anywhere — log once per run so users notice
		// misconfiguration instead of silently dropping notifications.
		slog.Warn("cron notify enabled but no target configured",
			"hint", "set cron.notify_default.platform + chat_id, or provide per-job notify_platform + notify_chat_id")
		return NotifyTarget{}
	}

	// Legacy default (notify==nil): IM-created jobs reply to their source chat.
	// Platform "dashboard" has no registered platform object so this naturally
	// no-ops for dashboard jobs that predate the toggle.
	if platName != "" && chatID != "" {
		return NotifyTarget{Platform: platName, ChatID: chatID}
	}
	return NotifyTarget{}
}

// deliverNotice sends a result/error message to the resolved target.
// No-op when target is unset or the platform is not registered.
func (s *Scheduler) deliverNotice(target NotifyTarget, text string) {
	if !target.IsSet() {
		return
	}
	s.notifyTarget(target.Platform, target.ChatID, text)
}

// recordResult persists the last execution result on the job and invokes the onExecute callback.
//
// sessionID 是本次执行从 CLI 拿到的 Claude session_id。成功路径传非空值，
// 错误路径（work_dir/session/send error）传空串：空值分支不会触碰
// j.LastSessionID，保留上一次成功执行留下的 ID，这样 dashboard 点击
// cron 侧边栏仍然能按历史 ID 拉到 JSONL 内容而不是空白面板。
func (s *Scheduler) recordResult(j *Job, result, errMsg, sessionID string) {
	// textutil.TruncateRunesNoEllipsis avoids the two O(n) rune-slice
	// allocations that `string([]rune(result)[:maxStoredResultRunes])`
	// performs and keeps the cron-specific "…[truncated]" suffix
	// (TruncateRunes appends "..." which is not what dashboard rendering
	// expects). Length compare is the O(1) truncation detector:
	// TruncateRunesNoEllipsis returns either the input unchanged or a
	// strictly shorter byte-length prefix, so any length drop signals
	// truncation actually happened. R219-CR-2.
	if shaped := textutil.TruncateRunesNoEllipsis(result, maxStoredResultRunes); len(shaped) < len(result) {
		result = shaped + truncatedSuffix
	}
	// Redact absolute filesystem paths from errMsg before persisting to
	// cron_jobs.json and broadcasting to all authenticated dashboard
	// clients. session.GetOrCreate / session.Send surface workspace-bearing
	// errors that would otherwise enumerate operator filesystem layout on
	// disk (at 0600) and in every connected browser's event stream. Keep
	// the structural prefix ("session error: …" / "send error: …") so the
	// error class remains obvious. R61-SEC-8.
	errMsg = redactPathsInCronError(errMsg)
	// R177-SEC-1: AI-authored result text and error strings may contain C1
	// controls, bidi overrides, LS/PS, or embedded newlines (especially
	// when the user prompt tricks the agent into echoing attacker-supplied
	// strings). Those flow into (a) cron_jobs.json on disk, (b) the
	// cronResultMsg WS broadcast to every authenticated dashboard client,
	// and (c) any future slog attr that logs j.LastResult — each is a
	// log-injection / stored-UI-spoofing vector. Apply the same
	// SanitizeForLog gate used on remote workspace / feishu nonce paths.
	// The length caps below double up with the rune truncation above;
	// SanitizeForLog's cap is byte-counted, so extend the result cap by
	// the suffix length so an already-truncated result keeps the
	// "…[truncated]" marker intact instead of having it byte-clipped.
	// R232-PERF-9.
	result = osutil.SanitizeForLog(result, maxStoredResultRunes+len(truncatedSuffix))
	errMsg = osutil.SanitizeForLog(errMsg, 512)
	s.mu.Lock()
	// If the job was deleted between execute()'s snapshot and recordResult's
	// write-back, skip both the persist and the onExecute callback: broadcasting
	// a "completed" result for an already-deleted job can flash a stale row in
	// the dashboard (Round 49 HIGH-4) and persists nothing useful since the job
	// is already gone from s.jobs.
	if _, ok := s.jobs[j.ID]; !ok {
		s.mu.Unlock()
		return
	}
	// RNEW-011: snapshot the four fields we're about to overwrite so marshal
	// failure can roll back in-memory state. Without rollback the live WS
	// broadcast and the on-disk snapshot diverge: dashboard shows "succeeded
	// at T" while cron_jobs.json still points at the previous run, and a
	// restart replays the stale result as authoritative. We keep the rollback
	// inside s.mu so the broadcast (fn, fired after Unlock) always sees either
	// the fully-applied new values (persist OK) or the fully-restored old ones
	// (persist failed) — never a half-applied mix.
	prevLastRunAt := j.LastRunAt
	prevLastResult := j.LastResult
	prevLastError := j.LastError
	prevLastSessionID := j.LastSessionID

	j.LastRunAt = time.Now()
	j.LastResult = result
	j.LastError = errMsg
	if sessionID != "" {
		j.LastSessionID = sessionID
	}
	save, perr := s.persistJobsLocked()
	if perr != nil {
		// Marshal failed: revert the four fields so in-memory and disk agree.
		// slog was already emitted by persistJobsLocked; add one more line so
		// operators can correlate rollback with broadcast suppression.
		j.LastRunAt = prevLastRunAt
		j.LastResult = prevLastResult
		j.LastError = prevLastError
		j.LastSessionID = prevLastSessionID
		s.mu.Unlock()
		slog.Warn("cron: recordResult persist failed; in-memory result reverted",
			"job_id", j.ID, "err", perr)
		return
	}
	s.mu.Unlock()

	save()
	if fn := s.onExecute.Load(); fn != nil {
		(*fn)(j.ID, result, errMsg)
	}
}

// redactPathsInCronError strips absolute filesystem paths from a cron
// execution error message before persistence. session.GetOrCreate and
// session.Send produce errors like "session error: workspace …/repo/x:
// permission denied" that would otherwise enumerate the operator's
// filesystem layout to every authenticated dashboard viewer and any
// cron_jobs.json backup reader. We replace both POSIX and Windows-style
// absolute paths with a literal "<path>" placeholder; error classification
// (permission denied, no such file) stays intact because the surrounding
// tokens aren't paths. R61-SEC-8.
//
// The implementation is a token-wise scan rather than a regex to avoid
// pulling a regex compile onto every cron run: recordResult is invoked on
// every execution and the regex cost would dominate the redaction budget.
func redactPathsInCronError(s string) string {
	if s == "" {
		return s
	}
	const maxErrLen = 2048
	if len(s) > maxErrLen {
		s = s[:maxErrLen] + "…"
	}
	// Fast path: if the string contains no POSIX slash and no Windows
	// backslash, there is nothing path-shaped to redact — skip the Builder
	// allocation and return the input unchanged. recordResult runs on every
	// cron execution, and common error classes ("dispatcher queue full",
	// "session error: context deadline exceeded") have no embedded paths.
	// R62-PERF-3.
	if strings.IndexByte(s, '/') < 0 && strings.IndexByte(s, '\\') < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		// POSIX absolute path: leading '/' followed by a non-space/non-quote
		// byte. Drive letter path C:\… also counts.
		isPosix := c == '/' && i+1 < len(s) && s[i+1] != ' ' && s[i+1] != '\t' && s[i+1] != '\n'
		isWin := i+2 < len(s) &&
			((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')) &&
			s[i+1] == ':' && (s[i+2] == '\\' || s[i+2] == '/')
		if !isPosix && !isWin {
			b.WriteByte(c)
			i++
			continue
		}
		// Consume the path until a delimiter that cannot appear in a
		// typical error-embedded path. Stopping at whitespace is the key
		// rule: error messages from the Go standard library spell paths
		// as tokens separated by whitespace ("open /tmp/x: reason"), and
		// the rare legitimate "path with space" in an error string is
		// vanishingly unlikely to survive redaction cleanly anyway. A
		// conservative scan errs on the side of over-redacting.
		j := i
		for j < len(s) {
			cc := s[j]
			if cc == '\n' || cc == ' ' || cc == '\t' || cc == ',' || cc == ';' ||
				cc == '\'' || cc == '"' || cc == '`' {
				break
			}
			if cc == ':' && j+1 < len(s) && (s[j+1] == ' ' || s[j+1] == '\n') {
				// `path: reason` — stop before the ':' so the reason tail
				// survives redaction.
				break
			}
			j++
		}
		b.WriteString("<path>")
		i = j
	}
	return b.String()
}

// notifyTarget sends a message to an arbitrary platform/chat (notify target).
func (s *Scheduler) notifyTarget(plat, chatID, text string) {
	p := s.platforms[plat]
	if p == nil {
		slog.Warn("cron notify: platform not found", "platform", plat)
		return
	}
	// Use Background parent: during shutdown stopCtx is cancelled first, then
	// cron.Stop() waits for in-flight jobs — those must still be able to deliver
	// their IM replies within the 30s bound rather than fail instantly.
	replyCtx, replyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer replyCancel()
	maxLen := p.MaxReplyLength()
	if maxLen <= 0 {
		maxLen = platform.DefaultMaxReplyLen
	}
	chunks := platform.SplitText(text, maxLen)
	for _, chunk := range chunks {
		if _, err := platform.ReplyWithRetry(replyCtx, p, platform.OutgoingMessage{
			ChatID: chatID,
			Text:   chunk,
		}, 3); err != nil {
			slog.Warn("cron notify target failed", "platform", plat, "chat", chatID, "err", err)
		}
	}
}

// findByPrefix finds a job by ID prefix scoped to a specific chat.
func (s *Scheduler) findByPrefix(idPrefix, plat, chatID string) (*Job, error) {
	var matches []*Job
	for _, j := range s.jobs {
		if j.Platform == plat && j.ChatID == chatID && strings.HasPrefix(j.ID, idPrefix) {
			matches = append(matches, j)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("%w: prefix %q", ErrJobNotFound, idPrefix)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}
		return nil, fmt.Errorf("ambiguous prefix %q, matches: %s", idPrefix, strings.Join(ids, ", "))
	}
}

// marshalJobsFn is the signature of the JSON serializer used by
// marshalJobsLocked. It is swapped via atomic.Pointer in tests (see
// withFailingMarshal) to exercise persist-failure paths without constructing
// a cyclic graph in Job. Kept behind an atomic.Pointer because other cron
// tests in the same package run with t.Parallel(); a naked var swap races
// with concurrent marshalJobsLocked readers under -race.
type marshalJobsFn func(any) ([]byte, error)

var marshalJobs atomic.Pointer[marshalJobsFn]

func init() {
	fn := marshalJobsFn(json.Marshal)
	marshalJobs.Store(&fn)
}

// marshalJobsLocked serialises the current jobs map to JSON while the caller
// still holds s.mu. Round 47: replaces the map clone on every mutation. Safe
// because json.Marshal only reads Job fields (no mutation) and the output []byte
// is independent of s.jobs lifetime, so the caller can drop s.mu immediately.
// The (*Job).entryID field is unexported and therefore invisible to Marshal,
// so the runtime-only value never leaks into cron_jobs.json.
func (s *Scheduler) marshalJobsLocked() ([]byte, error) {
	entries := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		entries = append(entries, j)
	}
	// Sort by ID for deterministic on-disk order. Map iteration is random, so
	// identical in-memory state would produce diff-noisy JSON across saves —
	// breaking git audit of backed-up cron_jobs.json and making post-incident
	// diffs much harder to read.
	slices.SortFunc(entries, func(a, b *Job) int { return cmp.Compare(a.ID, b.ID) })
	return (*marshalJobs.Load())(entries)
}

// persistJobsLocked marshals under the caller's s.mu and writes asynchronously.
// Callers hold s.mu (write or read), invoke this to produce the byte payload
// and the save func, unlock, then call the save func. This keeps marshal
// latency in the critical section (needed for snapshot consistency) but moves
// disk I/O + storeMu contention outside.
//
// Return contract:
//   - On success, returns a non-nil save func and nil err. Caller must unlock
//     s.mu before invoking save() so disk I/O does not block the mutex.
//   - On marshal failure, returns (nil, ErrPersistFailed). Caller MUST plumb
//     the error back to the HTTP layer (e.g. map to 500) because the in-memory
//     mutation has already happened and is now unpersisted — a restart would
//     replay the prior on-disk state. marshal failure is only observable under
//     OOM or a broken Job schema; either way an alert-worthy event.
//
// R51-QUAL-001: previously this returned a no-op func on marshal failure,
// so every mutation appeared to succeed even when nothing reached disk.
func (s *Scheduler) persistJobsLocked() (func(), error) {
	data, err := s.marshalJobsLocked()
	if err != nil {
		slog.Error("marshal cron store", "err", err)
		return nil, fmt.Errorf("%w: %v", ErrPersistFailed, err)
	}
	// Capture a monotonic sequence number under s.mu so it totals-orders all
	// marshals with the snapshot state they represent. saveMarshaled skips
	// writes whose seq is older than what has already landed on disk —
	// closes R48-REL-PERSIST-ORDERING-RACE (Go sync.Mutex is not FIFO so a
	// later marshal can reach storeMu before an earlier one).
	seq := s.saveSeq.Add(1)
	return func() { s.saveMarshaledSeq(data, seq) }, nil
}

// saveMarshaledSeq is the mutation-path persist function. It skips the write
// if lastSavedSeq has already advanced past our seq — this happens when Go's
// sync.Mutex hands storeMu to a later writer (larger seq) before us, so our
// data is strictly stale and writing it would roll back the disk state.
// Note: lastSavedSeq is read+stored under storeMu (Load+Store pattern), not a
// CAS — storeMu serialises both the staleness check and the disk write so a
// later seq can never race past us between Load and Store. Closes R48-REL-
// PERSIST-ORDERING-RACE. R232-CR-11.
func (s *Scheduler) saveMarshaledSeq(data []byte, seq uint64) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	if s.storePath == "" {
		return
	}
	if last := s.lastSavedSeq.Load(); seq <= last {
		// A newer snapshot already won the storeMu race. Dropping our write
		// is safe — the newer payload already contains every field we would
		// have persisted (mutations under s.mu are linearised by s.mu, so
		// seq order matches state order).
		slog.Debug("cron save skipped: newer snapshot already saved",
			"our_seq", seq, "last_saved_seq", last)
		return
	}
	if err := osutil.WriteFileAtomic(s.storePath, data, 0600); err != nil {
		slog.Error("save cron store", "err", err, "disk_full", osutil.IsDiskFull(err))
		return
	}
	s.lastSavedSeq.Store(seq)
}

// applyJitter 在执行 cron job 前引入一段随机延迟，用来把"整点共振起跑"的
// CPU / API 峰值打散。窗口上界 = min(jitterMax, period/4)：
//   - 5m 周期 → 最多抖 75s（不蚕食 1m 节奏）
//   - 30m 周期 → 最多抖 7m30s
//   - 1h+ 周期 → 抖满 jitterMax（默认 2m）
//
// 无法解析 schedule 或 period<=0 时用 jitterMax 兜底。抖动尊重 ctx：
// Stop() / 进程关机期间 stopCtx 取消 → 立即返回（不再执行 job）。
//
// 用 math/rand/v2（per-goroutine 安全且无全局锁），安全性不敏感：
// 这里的随机只影响启动时刻分布，不是密码学用途。
func applyJitter(ctx context.Context, schedule string, jitterMax time.Duration) {
	if jitterMax <= 0 {
		return
	}
	window := jitterMax
	if period := schedulePeriod(schedule, time.Now()); period > 0 {
		if quarter := period / 4; quarter < window {
			window = quarter
		}
	}
	if window <= 0 {
		return
	}
	d := time.Duration(mrand.Int64N(int64(window)))
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// slogPrintfLogger satisfies the Printf interface that robfig/cron's
// PrintfLogger expects, routing every emitted line through slog instead of
// the standard log package.
//
// Observability note: robfig/cron wraps this via non-verbose PrintfLogger
// (logger.go:28 in the vendored lib) which compiles Info() out entirely
// when logInfo=false. SkipIfStillRunning calls Info (chain.go:88) and
// therefore never reaches Printf at all; only Error() lines do — i.e.
// recover-panic recoveries and schedule parse failures. Panic recoveries
// are logged at Error (a real fault); anything else stays at Warn so
// upstream library changes that route new events through Error remain
// visible without silently demoting them.
type slogPrintfLogger struct{}

func (slogPrintfLogger) Printf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	msg = strings.TrimRight(msg, "\n")
	// 同时匹配 "panic" 和 "recovered"：robfig/cron 的 recover chain 把
	// recovery 消息固定包含 "panic"（chain.go:30 "cron: panic running job:
	// %v\n%s"），但 upstream 措辞调整时 "recovered" 是更稳定的兜底标记，
	// 避免静默降级为 Warn 漏报真实故障。
	if strings.Contains(msg, "panic") || strings.Contains(msg, "recovered") {
		slog.Error("cron logger", "msg", msg)
		return
	}
	slog.Warn("cron logger", "msg", msg)
}
