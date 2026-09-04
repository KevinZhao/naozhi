package cron

// scheduler_config.go: cron sentinels, the SessionRouter consumer interface,
// SchedulerConfig + its defaulting helpers (applyDefaults / resolveAllowedRoot),
// the chatJobKey index key, and the cronConfigMaps snapshot.

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
)

// ErrJobNotFound is returned by lookup/mutation APIs when no cron job matches.
// Callers should use errors.Is(err, cron.ErrJobNotFound) instead of string matching.
var ErrJobNotFound = errors.New("cron: job not found")

// ErrSandboxWorkDir rejects the placement=sandbox × work_dir≠"" combination
// (agentcore-cloud-sandbox RFC §4.4 Phase 1 guardrail). Raised by both AddJob
// and UpdateJob so the combination fails at save time, not at the first run.
var ErrSandboxWorkDir = errors.New("cron: sandbox placement does not support work_dir (Phase 1)")

// ErrAmbiguousPrefix is returned by findByPrefixLocked when an ID prefix matches
// more than one job in the same chat scope; callers surface a "please
// disambiguate" hint instead of a generic not-found.
var ErrAmbiguousPrefix = errors.New("cron: ambiguous job prefix")

// ErrJobAlreadyPaused is returned by PauseJob when the target job is already
// paused. HTTP handlers map it to 409 Conflict (well-formed request, wrong state).
var ErrJobAlreadyPaused = errors.New("cron: job already paused")

// ErrJobNotPaused is returned by ResumeJob when the target job is not paused.
var ErrJobNotPaused = errors.New("cron: job not paused")

// ErrJobPaused is returned by TriggerNow when the target job is paused, so a
// manual trigger cannot silently run against the operator's pause intent.
var ErrJobPaused = errors.New("cron: job is paused")

// ErrJobNoPrompt is returned by TriggerNow when the target job has no prompt
// configured; dashboard handlers map it to HTTP 422.
var ErrJobNoPrompt = errors.New("cron: job has no prompt")

// ErrPromptAlreadySet is returned by SetJobPrompt when the target job already
// has a non-empty prompt. SetJobPrompt only auto-fills the FIRST prompt on a
// dashboard-created (paused, empty-prompt) job and never overwrites — that is
// UpdateJob's job (#1503). IM auto-save callers treat it as benign; callers
// that meant to mutate should use UpdateJob and may map this to HTTP 409.
var ErrPromptAlreadySet = errors.New("cron: job already has a prompt; use UpdateJob to change it")

// ErrPersistFailed is returned by mutation APIs when the post-mutation JSON
// serialisation fails. The in-memory state has already changed and cannot be
// rolled back, so callers MUST surface a 500-class error; a silent success
// would let a restart replay the mutation's inverse (e.g. resurrect a deleted job).
var ErrPersistFailed = errors.New("cron: persist jobs failed")

// errInvalidAttentionID is returned by the §7.4 queue helpers when a runID is
// not scheduler-generated hex (the IDs flow into filesystem paths and the
// broadcast, so they are shape-validated before use).
var errInvalidAttentionID = errors.New("cron sandbox: invalid attention run id")

// ErrJobNotSandbox is returned by ReplaySandboxRun when the target job is not
// at placement=sandbox: a local job has no snapshot to replay and no microVM
// to inject into (RFC §7.3). The dashboard maps it to 409 Conflict.
var ErrJobNotSandbox = errors.New("cron: job is not at sandbox placement")

// ErrNoSnapshot is returned by ReplaySandboxRun when the original run has no
// input snapshot on disk (snapshots disabled, or a GC'd blob) — without it
// there is no payload to re-inject (RFC §5.2).
var ErrNoSnapshot = errors.New("cron: run has no input snapshot to replay")

// ErrStopUnconfirmed is returned by ReplaySandboxRun when the §6.2 pre-replay
// StopSession did not confirm the original microVM is terminated; replaying
// anyway risks the double-run §6.2 exists to prevent (retry is safe).
var ErrStopUnconfirmed = errors.New("cron: original microVM termination unconfirmed; cannot replay safely")

// ErrSandboxUnavailable is returned by ReplaySandboxRun when sandbox placement
// is not configured (no runner wired). Distinct from ErrJobNotSandbox: the job
// IS a sandbox job, but this naozhi cannot run sandbox jobs right now.
var ErrSandboxUnavailable = errors.New("cron: sandbox placement not configured")

// ErrReplayInFlight is returned by ReplaySandboxRun when the job already has a
// run in flight (the per-job CAS slot is taken); the dashboard maps it to 409.
var ErrReplayInFlight = errors.New("cron: job already has a run in flight; cannot replay now")

// ErrSchedulerStopped is returned by Start() when the scheduler has already
// been Stop()'d: the instance is single-shot (Stop intentionally leaks wrapper
// goroutines on budget-exceed), so reviving it would accumulate orphans across
// lifecycles (#984). Distinguishes "already stopped" from a loadJobs failure.
var ErrSchedulerStopped = errors.New("cron: scheduler already stopped")

// SessionRouter is the subset of session.Router the cron Scheduler consumes
// (consumer-side interface): tests inject a fake, and any new s.router.X()
// call requires extending it — a compile error instead of silent surface
// growth. It uses only cron-local types (Session / SessionStatus) so cron
// never imports internal/session; the production wireup wraps the concrete
// session in cronSessionAdapter (internal/wireup/cron_router_adapter.go) (#752).
type SessionRouter interface {
	// RegisterCronStubWithChain creates (or refreshes) a suspended exempt
	// session entry (key "cron:<jobID>") so the job shows in the dashboard
	// sidebar before its first run. chainIDs 注入 stub 的 prevSessionIDs，让
	// fresh_context cron 每次 Reset 后的新 stub 仍能通过 historySource 查到上次
	// 运行的 JSONL 历史；空 / nil 等同无链。Cron passes at most 1 element (#768).
	RegisterCronStubWithChain(key, workspace, lastPrompt string, chainIDs []string)
	// Reset discards the session for the given key (fresh-mode cron jobs,
	// Delete/Rename flows). Contract: Reset MUST NOT block on in-flight
	// turns (one slow CLI turn would otherwise pin the whole tick loop), and
	// callers MUST NOT hold scheduler.mu — the router's Reset may fan out a
	// notifyChange that re-enters scheduler state and would deadlock.
	Reset(key string)
	// GetOrCreate returns an existing session or spawns a new one at
	// execute time, as cron-local Session / SessionStatus types.
	GetOrCreate(ctx context.Context, key string, opts AgentOpts) (Session, SessionStatus, error)
}

// SchedulerConfig holds the value/scalar configuration for the cron
// scheduler; injected components live in SchedulerDeps (deps.go, #746). The
// scheduler stays struct-config rather than functional-options (#776), and
// the REQUIRED/OPTIONAL split below is API contract pinned by
// scheduler_config_construction_test.go.
//
// REQUIRED: SchedulerDeps.Router (no default; nil without AllowNilRouter →
// boot-time slog.Error). StorePath "" = in-memory only (valid for tests).
// OPTIONAL: MaxJobs <=0 → defaultMaxJobs, clamped to maxJobsHardCap;
// ExecTimeout <=0 → defaultExecTimeout; the rest per field (applyDefaults).
type SchedulerConfig struct {
	StorePath string
	MaxJobs   int
	// MaxJobsPerChat overrides DefaultMaxJobsPerChat when > 0. Zero/negative
	// fall back to the default deliberately, so operators cannot accidentally
	// disable the cap and let one chat starve the exempt-session pool.
	MaxJobsPerChat int
	ExecTimeout    time.Duration
	// Location is the timezone in which schedule expressions are evaluated;
	// nil = time.Local. DST caveats (robfig/cron v3): a spring-forward
	// expression in the skipped hour fires zero times that day; a fall-back
	// one may fire twice in the repeated hour (fast jobs are not protected by
	// SkipIfStillRunning). Prefer UTC for time-critical periodic work.
	Location *time.Location
	// NotifyDefault provides a fallback IM target for jobs that opt into
	// notifications (Job.Notify == true) but have no per-job target set.
	// Empty Platform or ChatID disables the default.
	NotifyDefault NotifyTarget
	// ParentCtx, if set, is the parent of the scheduler's internal stop
	// context, so cancelling it (application shutdown) interrupts running
	// cron jobs promptly. NewScheduler reads it once for context.WithCancel
	// and does not retain cfg, so a request- or test-scoped ctx may be
	// discarded right after NewScheduler returns (#846).
	ParentCtx context.Context
	// AllowedRoot mirrors Server.allowedRoot: the only directory tree under
	// which cron jobs may execute. Persisted jobs whose WorkDir falls outside
	// it are refused at Start() load time, so a tampered cron_jobs.json (or a
	// job persisted before AllowedRoot was configured) cannot escape the
	// sandbox at replay. Empty disables the check.
	AllowedRoot string
	// JitterMax is the upper bound of the randomized delay applied before
	// each scheduled tick; 0 disables jitter. The per-job window is clamped
	// to min(JitterMax, period/4) so short schedules are not swallowed.
	// TriggerNow bypasses jitter. See docs/rfc/cron-v2-polish.md §3.2.
	JitterMax time.Duration
	// RunsKeepCount overrides DefaultRunsKeepCount when > 0: the per-job cap
	// on retained run-history records (newest N kept). Zero (and negative)
	// values fall back to the default.
	RunsKeepCount int
	// RunsKeepWindow overrides DefaultRunsKeepWindow when > 0. Records
	// older than the window are trimmed at GC time. Zero (and negative)
	// values fall back to the default.
	RunsKeepWindow time.Duration
	// SlowThreshold overrides defaultCronSlowThreshold (30s) when > 0: a
	// successful execution exceeding it is counted as "slow"
	// (metrics.CronExecutionSlowTotal + warn). Raise it toward ExecTimeout
	// on deployments running 300s+ batch jobs so the daily false alarm stops
	// without losing the metric. Zero/negative fall back to the default (#519).
	SlowThreshold time.Duration
	// AllowNilRouter opts the constructor out of the boot-time "router
	// required" slog.Error (#510). Production always sets Router; the flag
	// lets in-package tests that never reach executeOpt / registerStub build
	// a Scheduler without a fakeRouter. When unset and Router is nil the error
	// fires at boot rather than as an opaque empty sidebar at runtime.
	AllowNilRouter bool
}

// chatJobKey identifies a (Platform, ChatID) pair for the per-chat job
// counter, making the maxJobsPerChat check one map lookup instead of an O(N)
// scan over s.jobs under s.mu (#661). Updates piggy-back on the already-locked
// s.mu sections (addJobAcquiringLock / deleteJobLocked / Start) so the counter
// never drifts from len-by-chat(s.jobs).
type chatJobKey struct {
	Platform string
	ChatID   string
}

// chatKeyFor builds the chatJobKey for a (platform, chatID) pair; the single
// constructor keeps the key shape in one place next to the jobsByChat /
// chatJobCount trio it indexes (#948, #1368).
func chatKeyFor(plat, chatID string) chatJobKey {
	return chatJobKey{Platform: plat, ChatID: chatID}
}

// maxJobsHardCap caps user-configurable MaxJobs: 500 jobs ≈ 500 tick timers,
// well within robfig/cron's scale, but higher values tend to indicate a
// config mistake. See docs/rfc/cron-v2-polish.md for sizing rationale.
const maxJobsHardCap = 500

// defaultMaxJobs is the fallback for SchedulerConfig.MaxJobs when the operator
// leaves it zero/negative; sized for typical single-tenant deployments.
const defaultMaxJobs = 50

// defaultExecTimeout bounds a single job execution when the operator leaves
// SchedulerConfig.ExecTimeout zero. 5 min covers nearly all CLI turn budgets
// without leaving runaway jobs holding the per-job overlap gate forever.
const defaultExecTimeout = 5 * time.Minute

// DefaultMaxJobsPerChat bounds how many cron jobs a single chat (platform +
// chat_id) may own so one loud group cannot consume the global MaxJobs quota
// nor the session.maxCronExempt sub-quota (each job holds one exempt stub).
// Overridable via SchedulerConfig.MaxJobsPerChat; zero falls back here — the
// cap cannot be disabled. See docs/rfc/cron-v2-polish.md for sizing rationale.
const DefaultMaxJobsPerChat = 10

// applyDefaults fills in zero-valued fields with their package-level defaults
// and clamps oversized values. Idempotent — a no-op on an already-defaulted
// config. Pointer receiver mutates in place; copy first if the original must
// be preserved.
func (cfg *SchedulerConfig) applyDefaults() {
	if cfg.MaxJobs <= 0 {
		cfg.MaxJobs = defaultMaxJobs
	}
	if cfg.MaxJobs > maxJobsHardCap {
		cfg.MaxJobs = maxJobsHardCap
	}
	// <= 0 maps to the default so a zero struct field cannot silently
	// disable the per-chat cap.
	if cfg.MaxJobsPerChat <= 0 {
		cfg.MaxJobsPerChat = DefaultMaxJobsPerChat
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = defaultExecTimeout
	}
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
}

// resolveAllowedRoot sanitises cfg.AllowedRoot (clearing NUL-bearing values
// loudly) and returns its EvalSymlinks-resolved form; empty on either side
// means "no root constraint". Kept off applyDefaults because EvalSymlinks is
// a syscall. A NUL byte would tokenise workDirCacheKeySuffix incorrectly and
// alias unrelated workDirs onto one cache slot, so it is cleared (#1297).
func (cfg *SchedulerConfig) resolveAllowedRoot() string {
	if strings.ContainsRune(cfg.AllowedRoot, 0) {
		slog.Error("cron.NewScheduler: cfg.AllowedRoot contains NUL byte; clearing to disable root constraint",
			"allowed_root_len", len(cfg.AllowedRoot))
		cfg.AllowedRoot = ""
	}
	if cfg.AllowedRoot == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(cfg.AllowedRoot); err == nil {
		return r
	} else {
		// EvalSymlinks failed (path missing / dangling symlink). Fall back to
		// the raw string with a Warn: bare comparison is weaker (symlinks under
		// the root could escape) but better than silently disabling the constraint.
		slog.Warn("cron.NewScheduler: filepath.EvalSymlinks failed for AllowedRoot; "+
			"using raw path (symlinks under this root may bypass the constraint)",
			"allowed_root", cfg.AllowedRoot, "err", err)
		return cfg.AllowedRoot
	}
}

// cronConfigMaps bundles the write-once-then-immutable config maps so a single
// atomic.Pointer publishes them as one consistent snapshot (#991): readers
// treat the maps as read-only; a hot-reload writer must build a fresh
// *cronConfigMaps (copy-on-write) and Store() it, so no reader sees a torn state.
type cronConfigMaps struct {
	// notifySender shares this atomic snapshot with agents/agentCommands
	// because notifyTarget reads it via s.configMaps() without s.mu; a
	// separate field could yield a torn cross-field read. Interface value,
	// write-once, so unlike the maps it is not cloned (#725).
	notifySender  NotifySender
	agents        map[string]AgentOpts
	agentCommands map[string]string
}

// emptyConfigMaps is the non-nil zero-value snapshot configMaps() returns for
// a hand-constructed *Scheduler (test fixtures) that never Store()d a pointer;
// indexing its nil maps is a safe zero-value read.
var emptyConfigMaps = &cronConfigMaps{}

// configMaps returns the current immutable config-map snapshot. Never nil: a
// *Scheduler built via &Scheduler{} (tests) gets emptyConfigMaps so the
// lock-free readers (notifyTarget / executeOpt) never nil-deref; the maps
// inside may be nil (maps.Clone(nil) == nil), which indexes safely.
func (s *Scheduler) configMaps() *cronConfigMaps {
	if cm := s.configMapsPtr.Load(); cm != nil {
		return cm
	}
	return emptyConfigMaps
}
