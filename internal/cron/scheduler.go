package cron

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	robfigcron "github.com/robfig/cron/v3"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
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
	// RegisterCronStubWithChain creates (or refreshes) a suspended exempt
	// session entry so the cron job shows up in the dashboard sidebar before
	// its first run. Key is always "cron:<jobID>".
	//
	// chainIDs 注入一组 session-ID 链给 stub 的 prevSessionIDs，让
	// fresh_context cron 每次 Reset 后新建的 stub 仍能通过 historySource
	// 查到上一次成功运行留下的 JSONL 历史
	// （~/.claude/projects/<cwd>/<id>.jsonl）。chainIDs 为空 / nil 时
	// 等同于无链 stub 注册。
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

type (
	OnRunStartedFunc func(RunStartedEvent)

	OnRunEndedFunc func(RunEndedEvent)
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
	// reads platforms without s.mu and executeOpt reads agents without
	// s.mu. A future caller must NOT mutate these maps in place; if
	// dynamic backend/agent registration ever lands, switch to
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
	// started 用 CAS 保证 Start() 幂等。重复调用直接返回 nil 而不再 reset
	// startedAtNanos / 二次 spawn cold-start GC / 二次 cron.Start。R241-ARCH-2。
	started atomic.Bool
	// stopCtx is the scheduler's lifecycle context. Storing context in a
	// struct is usually an anti-pattern, but here execute() is invoked via
	// a callback from robfig/cron whose signature has no ctx parameter, so
	// the scheduler itself owns the root context so Stop() can cancel in-
	// flight executions. Callers outside execute() take ctx as an argument.
	stopCtx    context.Context
	stopCancel context.CancelFunc
	// R225-GO-5: callback fields accessed via atomic.Pointer so external
	// readers (emit{Started,Ended} / recordResultP0WithSanitised) don't need to
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

	// gcWG tracks the cold-start GC goroutine spawned by Start() so Stop()
	// waits for it to finish before persisting state. Without this, Stop's
	// final saveJobs / Append paths can race with trimAll's filesystem
	// mutations on the runs/ tree (R236-GO-01).
	gcWG sync.WaitGroup

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

	// storeDirOnce gates the one-time MkdirAll(filepath.Dir(storePath), 0700)
	// that hardens the cron_jobs.json parent dir against group-readable XDG
	// defaults. Idempotent — the saveMarshaledSeq hot path runs the dir-mode
	// clamp once per process. R235-SEC-6.
	storeDirOnce sync.Once

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

// maxJobsHardCap caps user-configurable MaxJobs to prevent accidental
// overload. 500 jobs ≈ 500 tick timers; well within robfig/cron's tested
// scale, but higher values tend to indicate a config mistake.
const maxJobsHardCap = 500

// defaultMaxJobs is the fallback for SchedulerConfig.MaxJobs when the operator
// leaves it zero/negative. Sized for typical single-tenant deployments; the
// hard cap above protects against runaway configs.
const defaultMaxJobs = 50

// defaultExecTimeout bounds a single job execution when the operator leaves
// SchedulerConfig.ExecTimeout zero. 5 min covers nearly all CLI turn budgets
// without leaving runaway jobs holding the per-job overlap gate forever.
const defaultExecTimeout = 5 * time.Minute

// DefaultMaxJobsPerChat bounds how many cron jobs a single chat (platform+
// chat_id pair) may own. Prevents one loud group from consuming the
// global MaxJobs quota. Exported so tests and docs can reference the
// value; operators can override per deployment via
// SchedulerConfig.MaxJobsPerChat (zero / unset falls back to this
// default — no way to "disable" the cap without rebuilding).
//
// Relationship to exempt pool (BL2 acknowledged design):
// Every cron job calls session.Router.RegisterCronStubWithChain at scheduler
// Start / AddJob time and consumes 1 slot from session.maxExemptSessions
// (currently 20). At DefaultMaxJobsPerChat=10 × 2 busy chats, the exempt
// pool is fully consumed and planner/scratch exempt sessions may be
// starved. This is an acknowledged trade-off: a separate
// maxCronExemptSessions reserve or per-chat fair-share eviction is the
// escape hatch if pressure materialises.
const DefaultMaxJobsPerChat = 10

// workDirReachable reports whether workDir exists and resolves to a
// directory right now. Used before fresh-mode Reset so a job whose
// workspace has been deleted by an operator does not destroy the
// existing session just to fail on a GetOrCreate / spawn-shim call.
// Empty workDir means "use router default" and is always reachable.
// CRON2.
//
// 注意：workDirReachable 仅做 stat 可达性 + IsDir 检查，**不**强制
// allowedRoot 内含。任何依赖"必须在工作根之内"的调用者必须额外调
// workDirUnderRoot。当前调用点 (freshContextPreflightP0) 依赖
// loadJobs 阶段已做过 root-containment 校验；不要在不调
// workDirUnderRoot 的新调用点直接复用本函数。R234-CR-11。
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
// applyDefaults fills in zero-valued fields with their package-level
// defaults and clamps oversized values. R232-ARCH-14: extracted from
// NewScheduler so callers (especially tests that build SchedulerConfig
// directly) can see the full default set in one place rather than tracing
// through the constructor body. Returns the same struct by value so the
// caller can decide whether to use the resolved or original config.
//
// Idempotent — calling it on an already-defaulted config is a no-op.
func (cfg SchedulerConfig) applyDefaults() SchedulerConfig {
	if cfg.MaxJobs <= 0 {
		cfg.MaxJobs = defaultMaxJobs
	}
	if cfg.MaxJobs > maxJobsHardCap {
		cfg.MaxJobs = maxJobsHardCap
	}
	// Resolve per-chat cap: <= 0 maps to the default so a zero struct
	// field cannot silently disable the cap. R208-BL2.
	if cfg.MaxJobsPerChat <= 0 {
		cfg.MaxJobsPerChat = DefaultMaxJobsPerChat
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = defaultExecTimeout
	}
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	return cfg
}

func NewScheduler(cfg SchedulerConfig) *Scheduler {
	before := cfg.MaxJobs
	if before > maxJobsHardCap {
		slog.Warn("cron max_jobs exceeds hard cap, clamping", "requested", before, "cap", maxJobsHardCap)
	}
	cfg = cfg.applyDefaults()
	maxPerChat := cfg.MaxJobsPerChat
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
	// applyDefaults guarantees Location is non-nil (defaults to time.Local).
	loc := cfg.Location
	// R232-CR-4: surface "general" fallback being absent. ResolveAgent returns
	// "general" when the prompt has no slash-prefix; if that agent isn't
	// configured, executeOpt reads a zero AgentOpts (empty Backend / Model /
	// SystemPromptFile) and the cron tick spawns with backend defaults
	// silently. Logging at construction makes the misconfiguration visible
	// without changing runtime behaviour.
	if _, ok := cfg.Agents["general"]; !ok {
		slog.Debug("cron: 'general' agent missing from agents map; cron jobs without slash-prefix will fall back to backend defaults",
			"agent_count", len(cfg.Agents))
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
//
// Idempotency (R241-ARCH-2): a second Start() returns nil immediately
// without re-loading jobs, re-spawning the cold-start GC pass, or
// re-invoking robfig/cron's Start (which would panic on a running
// runner). The CAS guard runs *before* startedAtNanos.Store so a
// double-Start does not reshape the missed-schedule suppression
// window mid-flight.
func (s *Scheduler) Start() error {
	if !s.started.CompareAndSwap(false, true) {
		return nil
	}
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
		// R241-ARCH-2: on load failure, release the idempotency latch so
		// the operator can retry Start() after fixing the store file.
		// startedAtNanos already follows the same retry contract (see
		// comment above its Store).
		s.started.Store(false)
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
				"job_id", j.ID, "work_dir", j.WorkDir)
			continue
		}
		if j.Paused {
			s.jobs[j.ID] = j
			stubs = append(stubs, stubRow{j.ID, j.WorkDir, j.Prompt, j.LastSessionID})
			continue
		}
		if err := s.registerJob(j); err != nil {
			slog.Warn("skip invalid cron job", "job_id", j.ID, "schedule", j.Schedule, "err", err)
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
		s.gcWG.Add(1)
		go func() {
			defer s.gcWG.Done()
			slog.Info("cron run history: cold-start GC starting")
			s.runStore.trimAll(time.Now())
			slog.Info("cron run history: cold-start GC done")
		}()
	}
	slog.Info("cron scheduler started", "jobs", jobCount)
	return nil
}

// registerStubByValue creates (or refreshes) a router session entry for the
// job so it appears in the dashboard workspace list. Safe to call without a
// router (tests). Callers must not be holding s.mu — RegisterCronStubWithChain
// re-enters router state.
//
// 当 lastSessionID 非空（最近一次成功执行的 session_id），会作为单元素
// chain 传给 stub，这样 dashboard 点击 cron 侧边栏时能按该 ID 从 claude
// 项目目录找到 JSONL 历史。否则 fresh_context=true 的定时任务每次 Reset
// 都会把 stub 的 chain 清空，事件面板就永远是空白。
//
// R232-CR-12 把原 registerStub(*Job) / registerStubByValue / stubChain 三
// 个仅参数差异的 helper 合成单个值参数版本：避免持锁路径误传 *Job 指针
// 后被并发 UpdateJob 改动；调用方一律先快照字段再传值。
func (s *Scheduler) registerStubByValue(id, workDir, prompt, lastSessionID string) {
	if s.router == nil {
		return
	}
	var chain []string
	if lastSessionID != "" {
		chain = []string{lastSessionID}
	}
	s.router.RegisterCronStubWithChain(session.CronKey(id), workDir, prompt, chain)
}

// registerStubFromJob 是 registerStubByValue 的便捷包装，对未持锁、且对
// *Job 字段稳定性已有把握（如 AddJob 后立刻调）的调用方简化字面。
func (s *Scheduler) registerStubFromJob(j *Job) {
	s.registerStubByValue(j.ID, j.WorkDir, j.Prompt, j.LastSessionID)
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

// gcWaitBudget bounds the cold-start GC goroutine wait in Stop(). Smaller
// than stopBudget because trimAll's IO is short-lived (ReadDir + N Removes);
// a wedge here means a stuck filesystem and we'd rather skip the wait than
// pin systemd TimeoutStopSec.
var gcWaitBudget = 5 * time.Second

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

	// R236-GO-01: wait for the cold-start GC goroutine spawned in Start()
	// before draining the cron scheduler. Without this, trimAll's filesystem
	// mutations on the runs/ tree race with Stop's final persist path and
	// any in-flight Append from a TriggerNow draining via triggerWG.Wait.
	// Bounded by a 5s timer so a wedged trimAll cannot pin Stop past
	// stopBudget — the goroutine is naturally short-lived (ReadDir + N
	// Removes), so any timeout here indicates a stuck filesystem.
	gcDone := make(chan struct{})
	go func() {
		s.gcWG.Wait()
		close(gcDone)
	}()
	gcTimer := time.NewTimer(gcWaitBudget)
	defer gcTimer.Stop()
	select {
	case <-gcDone:
	case <-gcTimer.C:
		slog.Warn("cron: gc goroutine wait timeout", "budget", gcWaitBudget)
	}

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

	// R232-ARCH-10: route shutdown save through persistJobsLocked so it
	// participates in the saveSeq monotonic gate. Without this, a queued
	// in-flight saveMarshaledSeq from a mutator that committed just before
	// Stop could acquire storeMu *after* Stop's bare WriteFileAtomic and
	// overwrite the freshest snapshot with stale data — Stop's bare write
	// did not bump lastSavedSeq, so the staleness check would let the
	// older write through.
	s.mu.Lock()
	save, err := s.persistJobsLocked()
	s.mu.Unlock()
	if err != nil {
		slog.Error("marshal cron store on shutdown", "err", err)
		return
	}
	if save != nil {
		save()
	}
}

// resetRouterStub is the deferred router-side cleanup that pairs with
// deleteJobLocked. Caller MUST NOT hold s.mu — router.Reset re-enters
// router state and its notifyChange callback may take s.mu. Safe on a
// nil router (tests). R240-GO-1.
func (s *Scheduler) resetRouterStub(jobID string) {
	if s.router == nil {
		return
	}
	s.router.Reset(session.CronKey(jobID))
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
