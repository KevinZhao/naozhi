package cron

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	robfigcron "github.com/robfig/cron/v3"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// Scheduler manages cron jobs and executes them on schedule.
//
// Field lifetime classes: lifecycle (cron / stopCtx / stopCancel / started /
// stopped / startedAtNanos / triggerWG / gcWG) written once at NewScheduler /
// Start / Stop, otherwise read-only or atomic; immutable config (router /
// configMapsPtr / location / notifyDefault / allowedRoot* / jitterMax /
// slowThreshold / execTimeout / maxJobs* / storePath / stopBudget) set in
// NewScheduler and read lock-free; mu-guarded mutable state (jobs /
// chatJobCount / jobsByChat / sortedJobIDs); independently synchronised state
// (runningJobs, telemetry, store*, *Once, sandboxPending*) with own primitives.
type Scheduler struct {
	cron *robfigcron.Cron
	// mu guards the jobs / chatJobCount / jobsByChat trio (RLock reads,
	// Lock writes). It does NOT cover the immutable-config fields (read
	// lock-free) nor the independently-synchronised fields (runningJobs /
	// telemetry / store* / *Once), each of which carries its own primitive.
	mu sync.RWMutex
	// 读写: 全部读写持 s.mu（读 RLock / 写 Lock）。jobs / chatJobCount /
	// jobsByChat 三者在同一把锁下同步变更，不会相互漂移。
	jobs map[string]*Job
	// chatJobCount tracks jobs per (Platform, ChatID). Maintained with s.jobs
	// writes under s.mu so the per-chat capacity check is O(1); entries are
	// deleted at zero so the working set tracks live chats (#661).
	chatJobCount map[chatJobKey]int
	// jobsByChat indexes *Job by (Platform, ChatID) so findByPrefixLocked
	// scans only the caller's chat (~O(5)) instead of all of s.jobs (O(500)).
	// Maintained with s.jobs writes under s.mu; entries deleted when the
	// slice empties. (Platform, ChatID) is immutable post-AddJob, so an entry
	// never moves across keys — add appends, delete swaps-and-shrinks (#558).
	jobsByChat map[chatJobKey][]*Job
	// sortedJobIDs mirrors the keys of s.jobs in ascending ID order,
	// maintained incrementally at the two s.mu seams that mutate s.jobs
	// (addToChatIndexLocked / deleteJobLocked) so persist avoids an
	// O(N log N) sort under s.mu (#1598). s.jobs stays the source of truth:
	// marshalJobsLocked validates this hint and rebuilds from the map on drift.
	sortedJobIDs []string
	// router is set once in NewScheduler and never reassigned.
	router SessionRouter
	// configMapsPtr publishes notifySender / agents / agentCommands as one
	// immutable *cronConfigMaps snapshot: notifyTarget and executeOpt read
	// them lock-free without s.mu, so a hot-reload writer must Store() a
	// freshly built struct (copy-on-write), never mutate the maps in place
	// (#991). Set once in NewScheduler and never swapped today.
	configMapsPtr atomic.Pointer[cronConfigMaps]
	storePath     string
	maxJobs       int
	// sandbox executes placement=sandbox jobs (agentcore-cloud-sandbox RFC
	// §4.2). Set once from SchedulerDeps.Sandbox in NewScheduler, never
	// reassigned — executeOpt reads it lock-free. nil = sandbox placement
	// unavailable (jobs fail with ErrClassCronSandboxUnavailable).
	sandbox SandboxRunner
	// maxJobsPerChat is the resolved per-chat cap: SchedulerConfig
	// MaxJobsPerChat when > 0, otherwise DefaultMaxJobsPerChat. Immutable
	// after NewScheduler returns, so AddJob can read it lock-free.
	maxJobsPerChat int
	execTimeout    time.Duration
	// stopBudget is the per-instance Stop() budget seeded from
	// defaultStopBudget. Per-instance (not a package var) so t.Parallel tests
	// shortening it on one *Scheduler cannot clobber another (#947). Read only
	// on the Stop() goroutine after construction, so no atomic; the test seam
	// WithStopBudgetField must run before the Scheduler is shared.
	stopBudget time.Duration
	// watchdogInterruptTimeoutNanos bounds sess.InterruptViaControl inside
	// runDeadlineWatchdog. Per-instance so parallel timeout tests stay
	// isolated (#1904); atomic because the AfterFunc watchdog goroutine reads
	// it concurrently with a test's Store. Seeded with
	// watchdogInterruptTimeoutDefault in NewScheduler.
	watchdogInterruptTimeoutNanos atomic.Int64
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
	// allowedRootResolved snapshots filepath.EvalSymlinks(allowedRoot) at
	// construction as a best-effort fallback for workDirUnderRoot when the
	// per-call EvalSymlinks on the root fails (e.g. temporarily unmounted).
	// The per-call resolve stays primary so TOCTOU protection against
	// root-side symlink swaps is preserved. Empty = fall back to raw allowedRoot.
	allowedRootResolved string
	// workDirCacheKeySuffix is "\x00" + allowedRoot + "\x00" +
	// allowedRootResolved, precomputed because both parts are immutable, so
	// workDirResolveUnderRootCached does one concat per tick, not three (#1225).
	workDirCacheKeySuffix string
	// jitterMax is the scheduling jitter cap. See SchedulerConfig.JitterMax.
	// Immutable after NewScheduler returns, so no lock needed.
	jitterMax time.Duration
	// slowThreshold is the wall-clock budget beyond which a successful run
	// counts as "slow"; zero/negative falls through to
	// defaultCronSlowThreshold at the callsite. Immutable after NewScheduler.
	slowThreshold time.Duration
	// routerNilOnce limits registerStubByValue's "router missing" slog.Error
	// to once per Scheduler lifetime, so a router-less fixture (or an
	// AllowNilRouter deployment) does not spam the log every tick; with
	// NewScheduler's boot-time error that is one loud signal at startup and
	// one at the first runtime callsite (#510).
	routerNilOnce sync.Once
	// startedAtNanos 是 Start() 被调用的时刻（UnixNano），用于 missed-schedule
	// 检测的启动抑制窗口（(now - startedAt) > 5×period 才算 missed）。用
	// atomic.Int64 是因为 Start() 写入与 dashboard 轮询 StartedAt() 读取无锁并发。
	startedAtNanos atomic.Int64
	// started 用 CAS 保证 Start() 幂等。重复调用直接返回 nil 而不再 reset
	// startedAtNanos / 二次 spawn cold-start GC / 二次 cron.Start。
	started atomic.Bool
	// stopped 用 CAS 保证 Stop() 幂等：重复调用直接 return，避免二次 NewTimer、
	// 二次 persistJobsLocked 落盘竞争、二次 cron.Stop()。stopCancel 本身已幂等。
	stopped atomic.Bool
	// stopCtx is the scheduler's lifecycle context: execute() is a robfig/cron
	// callback with no ctx parameter, so the scheduler owns the root ctx that
	// Stop() cancels. Reads are confined to callback-derived paths; a method
	// that already receives a ctx must NOT use this field (#972). Derived from
	// ParentCtx, so it is the single cancel signal — never read ParentCtx (#974).
	stopCtx    context.Context
	stopCancel context.CancelFunc
	// telemetry receives cron-run lifecycle events. atomic.Pointer because
	// SetTelemetry can land after tick goroutines already read it in
	// emitRunStarted / emitRunEnded. Broadcaster is an interface, so we store
	// *Broadcaster and Load + deref; nil pointer == no broadcaster.
	telemetry atomic.Pointer[runtelemetry.Broadcaster]

	// triggerWG tracks goroutines spawned by TriggerNow so Stop() can wait
	// for them to finish. The scheduled entries are already drained by
	// s.cron.Stop(), but manual TriggerNow fires a goroutine outside the
	// cron scheduler's purview.
	triggerWG sync.WaitGroup

	// gcWG tracks the cold-start GC goroutine spawned by Start() so Stop()
	// waits for it before persisting; otherwise the final saveJobs / Append
	// paths race trimAll's filesystem mutations on the runs/ tree.
	gcWG sync.WaitGroup

	// runningJobs serializes execute(j) per job ID so a manual TriggerNow
	// cannot overlap a scheduled tick (SkipIfStillRunning only covers the
	// scheduled path). Entries are deliberately NOT cleared on job delete: a
	// concurrent execute() may still hold the *runInflight CAS gate, and a
	// reused ID would split the gate and allow double execution (leak ≤ maxJobsHardCap).
	runningJobs sync.Map // map[jobID]*runInflight

	// jobGates shards a fixed pool of mutexes (hashed jobID) that serialise
	// executeOpt's jobInflight load→CAS against cleanupRunningJobIfIdle's
	// load→CompareAndDelete for the same job, closing the TOCTOU window where
	// DeleteJob racing TriggerNow orphans the CAS gate (#1706). See job_gate.go.
	jobGates [jobGateShards]sync.Mutex

	// storeMu serialises saveSnapshot writes so last-writer-wins order matches
	// the order snapshots were marshaled under s.mu. WriteFileAtomic uses a
	// unique temp file per call, so this is only a logical barrier against an
	// older snapshot rename-winning. Held only around WriteFileAtomic in
	// saveMarshaledSeq; snapshot construction stays on s.mu.
	storeMu sync.Mutex

	// storeDirOnce gates the one-time MkdirAll(filepath.Dir(storePath), 0700)
	// that hardens the cron_jobs.json parent dir against group-readable XDG
	// defaults, so the saveMarshaledSeq hot path clamps once per process.
	storeDirOnce sync.Once

	// saveSeq tags every marshaled snapshot at capture time (under s.mu);
	// saveMarshaled skips the write under storeMu if lastSavedSeq is already
	// newer. sync.Mutex is not FIFO, so an older snapshot could otherwise
	// reach storeMu after a newer one and overwrite it on disk; the seq gate
	// makes saveMarshaled idempotent w.r.t. stale payloads.
	saveSeq      atomic.Uint64 // assigned while holding s.mu
	lastSavedSeq atomic.Uint64 // read/CAS'd while holding storeMu

	// runStore persists a CronRun record per terminal execution (P1
	// cron-run-history). nil-safe: empty StorePath disables persistence
	// transparently (tests / no-disk deployments).
	runStore *runStore

	// sandboxPendingMu guards sandboxPendingIndex, independent of s.mu so the
	// hot delete path never contends with job CRUD. RWMutex so the pure-read
	// lookup (lookupSandboxPendingIndex) does not serialize against reads.
	sandboxPendingMu sync.RWMutex
	// sandboxPendingIndex maps jobID → its in-flight §6.5 pending file path
	// (set by writeSandboxPending, cleared at terminal remove) so
	// stopSandboxRunsForJob resolves a deleted job's file with one lookup
	// instead of scanning every run record (#2140). Tracks only records THIS
	// process wrote; orphans from a previous boot go through reconcileSandboxPending.
	sandboxPendingIndex map[string]string

	// workDirCache memoises positive workDirResolveUnderRoot results so
	// fast-firing jobs do not repeat the EvalSymlinks chain every tick.
	// TTL-bounded (workDirResolveCacheTTL) so symlink retargets surface soon.
	workDirCache workDirResolveCache

	// workDirReachableCache memoises positive workDirReachable() results so
	// fresh-mode jobs with allowedRoot=="" (which never touch workDirCache)
	// do not os.Stat every tick. Same TTL and positive-only semantics: an
	// unreachable result bypasses the cache so a restored workspace surfaces
	// on the next tick. Keyed by raw workDir (#1731).
	workDirReachableCache workDirResolveCache

	// knownSessionsCache memoises KnownSessionIDs() for knownSessionsCacheTTL:
	// the dashboard polls it at 1Hz per tab and a rebuild walks every job's
	// runStore.Recent. Invalidated explicitly on writes that can change the
	// set (LastSessionID assignment, runStore.Append).
	knownSessionsCache knownSessionsCache

	// marshalJobs is the JSON serializer used by marshalJobsLocked, behind
	// atomic.Pointer so tests (withFailingMarshal) can swap a failing stub
	// without racing concurrent persist readers under -race. Per-Scheduler
	// (not a package var) so parallel tests cannot leak a stub across
	// schedulers; initialised to defaultMarshalJobs in NewScheduler (#693).
	marshalJobs atomic.Pointer[marshalJobsFn]

	// clock is the time source for lifecycle timestamps (run finish endedAt,
	// synthetic-skipped startedAt); tests inject a fake to pin durations
	// without sleeping. Read via s.now() so a zero-value Scheduler falls back
	// to wall-clock time instead of nil-panicking.
	clock cronClock

	// finishRunPreAppendHook is a test-only seam invoked by finishRun after
	// recordTerminalResult releases s.mu and before the jobStillExists
	// re-check gating the runs/<jobID>/ write (#2058), so a test can land a
	// DeleteJobByID deterministically inside that window (#2473). Always nil
	// in production; set only before the scheduler is shared across goroutines.
	finishRunPreAppendHook func(jobID string)
}

// NewScheduler creates a scheduler. Call Start() to begin. cfg carries the
// value/scalar configuration; deps carries the injected components (cfg/deps
// split per RFC cron-sysession-merge §3.5.1, #746 — see deps.go).
func NewScheduler(cfg SchedulerConfig, deps SchedulerDeps) *Scheduler {
	// Surface missing router wiring at boot (slog.Error, not panic: many
	// in-package tests build router-less Schedulers for paths that never
	// reach executeOpt, and executeOpt's own nil guard short-circuits the
	// hot path). Tests that want silence opt in via AllowNilRouter (#510).
	if deps.Router == nil && !cfg.AllowNilRouter {
		slog.Error("cron.NewScheduler: deps.Router is nil; dashboard sidebar entries will not be created and executeOpt will short-circuit. Set SchedulerDeps.Router on the production wireup, or SchedulerConfig.AllowNilRouter=true on tests that intentionally exercise router-less paths.")
	}
	before := cfg.MaxJobs
	if before > maxJobsHardCap {
		slog.Warn("cron max_jobs exceeds hard cap, clamping", "requested", before, "cap", maxJobsHardCap)
	}
	cfg.applyDefaults()
	maxPerChat := cfg.MaxJobsPerChat
	parent := cfg.ParentCtx
	if parent == nil {
		parent = context.Background()
	}
	stopCtx, stopCancel := context.WithCancel(parent)
	// NUL-byte sanitisation + EvalSymlinks of AllowedRoot live in a helper
	// (mutates cfg.AllowedRoot in place), separate from applyDefaults because
	// EvalSymlinks is a syscall and applyDefaults is pure / idempotent (#517).
	allowedRootResolved := cfg.resolveAllowedRoot()
	cronLogger := robfigcron.PrintfLogger(slogPrintfLogger{})
	// applyDefaults guarantees Location is non-nil (defaults to time.Local).
	loc := cfg.Location
	// Surface a missing "general" agent: ResolveAgent returns "general" for
	// prompts without a slash-prefix, and if it isn't configured executeOpt
	// reads a zero AgentOpts and spawns with backend defaults silently.
	if _, ok := deps.Agents["general"]; !ok {
		slog.Debug("cron: 'general' agent missing from agents map; cron jobs without slash-prefix will fall back to backend defaults",
			"agent_count", len(deps.Agents))
	}
	s := &Scheduler{
		cron: robfigcron.New(
			robfigcron.WithLocation(loc),
			robfigcron.WithChain(
				robfigcron.Recover(cronLogger),
				robfigcron.SkipIfStillRunning(cronLogger),
			),
		),
		jobs:         make(map[string]*Job),
		chatJobCount: make(map[chatJobKey]int),
		jobsByChat:   make(map[chatJobKey][]*Job),
		router:       deps.Router,
		// notifySender / agents / agentCommands are published via configMapsPtr below.
		storePath:      cfg.StorePath,
		maxJobs:        cfg.MaxJobs,
		maxJobsPerChat: maxPerChat,
		execTimeout:    cfg.ExecTimeout,
		sandbox:        deps.Sandbox,
		// Seeded from the const; tests override per-instance via
		// WithStopBudgetField so t.Parallel Stops cannot race a global (#947).
		stopBudget:          defaultStopBudget,
		location:            loc,
		notifyDefault:       cfg.NotifyDefault,
		allowedRoot:         cfg.AllowedRoot,
		allowedRootResolved: allowedRootResolved,
		// Must stay byte-for-byte identical to workDirResolveCacheKey(workDir,
		// allowedRoot, allowedRootResolved) minus the leading workDir,
		// otherwise cache lookups miss (#1225).
		workDirCacheKeySuffix: "\x00" + cfg.AllowedRoot + "\x00" + allowedRootResolved,
		jitterMax:             cfg.JitterMax,
		slowThreshold:         cfg.SlowThreshold,
		stopCtx:               stopCtx,
		stopCancel:            stopCancel,
		runStore:              newRunStore(cfg.StorePath, cfg.RunsKeepCount, cfg.RunsKeepWindow),
		sandboxPendingIndex:   make(map[string]string),
		// Tests swap a fake via the withClock seam.
		clock: defaultClock,
	}
	// Seed so marshalJobsLocked never Load()s nil.
	s.marshalJobs.Store(&defaultMarshalJobs)
	s.watchdogInterruptTimeoutNanos.Store(int64(watchdogInterruptTimeoutDefault))
	// maps.Clone severs the alias to the caller-supplied maps so post-Start
	// mutation of deps.* cannot race the lock-free readers (#506, #991).
	s.configMapsPtr.Store(&cronConfigMaps{
		// interface value, no backing array to alias — stored as-is (#725)
		notifySender:  deps.NotifySender,
		agents:        maps.Clone(deps.Agents),
		agentCommands: maps.Clone(deps.AgentCommands),
	})
	// nil deps.Telemetry leaves the pointer zero-valued (no broadcast).
	if deps.Telemetry != nil {
		b := deps.Telemetry
		s.telemetry.Store(&b)
	}
	// Eagerly clamp the store parent dir to 0o700 at construction: the
	// storeDirOnce gate in saveMarshaledSeq only fires on the first save, so
	// otherwise the dir keeps its inherited XDG mode (often 0o755) until then
	// (#834). MkdirAll only sets perms when creating, so Chmod afterwards
	// covers a pre-existing dir (#830). Failures are logged and non-fatal.
	if cfg.StorePath != "" {
		s.storeDirOnce.Do(func() {
			if dir := filepath.Dir(cfg.StorePath); dir != "" && dir != "." {
				// MkdirAll failure is Error (not Warn): NewScheduler cannot return
				// an error, and a failed MkdirAll guarantees the first save will
				// ENOENT — better a boot-time signal than a runtime one (#1395).
				// Chmod failure stays Warn: recoverable by an operator chmod.
				if err := os.MkdirAll(dir, 0o700); err != nil {
					slog.Error("cron store parent dir mkdir failed; persistence will fail at first save",
						"err", err, "dir", dir)
				}
				if err := os.Chmod(dir, 0o700); err != nil && !errors.Is(err, fs.ErrNotExist) {
					slog.Warn("cron store parent dir chmod failed (eager)", "err", err, "dir", dir)
				}
			}
		})
	}
	return s
}

// NotifyDefault returns the configured fallback IM target so the dashboard
// can show where a "notify on completion" toggle will deliver. It is the
// NewScheduler-time snapshot of SchedulerConfig.NotifyDefault (no runtime
// mutation), so callers may cache it. Zero NotifyTarget when unset (check
// NotifyTarget.IsSet()). Nil-safe, like Location() / StartedAt(), so the
// dashboard can render a placeholder before the scheduler is wired.
func (s *Scheduler) NotifyDefault() NotifyTarget {
	if s == nil {
		return NotifyTarget{}
	}
	return s.notifyDefault
}

// StartedAt 返回 Scheduler 最近一次 Start() 的时刻。用于 missed-schedule
// 检测的启动抑制窗口。未 Start 前返回零值。
//
// Nil-safe (returns the zero time): the dashboard reads Location /
// NotifyDefault / StartedAt together during the bootstrap window, so none
// of the three may panic on a nil receiver (#955).
func (s *Scheduler) StartedAt() time.Time {
	if s == nil {
		return time.Time{}
	}
	ns := s.startedAtNanos.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// StartContext binds ctx to the scheduler's lifecycle and then calls Start.
// It is the idiomatic Start(ctx) entry point (#1168): a non-nil ctx's
// cancellation propagates INTO stopCtx exactly like ParentCtx does, so app
// shutdown interrupts running jobs via the same stopCtx.Done() signal Stop()
// drives, without constructing a SchedulerConfig just to thread the ctx.
//
// A nil ctx behaves identically to Start(). The watcher goroutine exits when
// EITHER ctx or stopCtx is done, so it never outlives the scheduler. Not
// meant to be combined with a separate Start() call; idempotent via the same
// started/stopped CAS guards.
func (s *Scheduler) StartContext(ctx context.Context) error {
	if ctx != nil {
		// Mirror ParentCtx's "cancel propagates into stopCtx" contract
		// without re-parenting stopCtx (which is created eagerly in
		// NewScheduler). A lightweight watcher cancels stopCtx when ctx
		// fires; it also drains on stopCtx so it cannot leak past Stop().
		go func() {
			select {
			case <-ctx.Done():
				s.stopCancel()
			case <-s.stopCtx.Done():
			}
		}()
	}
	return s.Start()
}

// Start loads persisted jobs and starts the cron scheduler.
//
// Idempotent: a second Start() returns nil immediately without re-loading
// jobs, re-spawning the cold-start GC pass, or re-invoking robfig/cron's
// Start (which would panic on a running runner). The CAS guard runs before
// startedAtNanos.Store so a double-Start does not reshape the
// missed-schedule suppression window mid-flight.
func (s *Scheduler) Start() error {
	// Refuse to (re)start once Stop() has latched: Stop intentionally leaks
	// wrapper goroutines on budget-exceed because the instance is single-shot,
	// and the started CAS alone does not block Start-after-Stop when a prior
	// Start failed at loadJobs and reset started=false (#984).
	if s.stopped.Load() {
		return ErrSchedulerStopped
	}
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
		// Release the idempotency latch so the operator can retry Start()
		// after fixing the store file. Clear nanos BEFORE started so a
		// concurrent retry that wins the CAS cannot observe started=true with
		// a stale startedAtNanos, and StartedAt() stays zero until a Start()
		// actually hands off to the cron runner.
		s.startedAtNanos.Store(0)
		s.started.Store(false)
		return fmt.Errorf("load cron store: %w", err)
	}

	s.mu.Lock()
	// Snapshot the fields passed to registerStub under lock so no *Job is
	// dereferenced after s.mu is released (a later UpdateJob could race).
	// lastSessionID 一起快照，重启后恢复的 cron stub 才能带上上次成功执行的
	// session_id，historySource 才能从 JSONL 把历史读回来给 dashboard 显示。
	type stubRow struct{ id, workDir, prompt, lastSessionID string }
	var stubs []stubRow
	// Enforce s.maxJobs during restore: an operator who lowered MaxJobs after
	// the on-disk store already exceeded it must not silently load every job
	// (#1187). Skipped jobs stay on disk (Start never persists), so raising
	// the cap and restarting recovers them; a WARN per skipped job names it.
	skippedOverCap := 0
	skippedOverPerChat := 0
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
		// Cap check fires AFTER the workDir filter so a sandbox-rejected
		// entry does not consume a cap slot.
		if len(s.jobs) >= s.maxJobs {
			slog.Warn("cron job over maxJobs cap; skipping (raise cron.MaxJobs to restore)",
				"job_id", j.ID, "schedule", j.Schedule, "cap", s.maxJobs)
			skippedOverCap++
			continue
		}
		// Enforce the per-chat cap on the load path too: a legacy /
		// hand-edited store with an over-cap chat would otherwise leave
		// chatJobCount above the cap and make AddJob report "limit reached"
		// while the operator believes there is headroom (#2060). Over-cap
		// entries stay on disk like the maxJobs skip above.
		if s.chatJobCount[chatKeyFor(j.Platform, j.ChatID)] >= s.maxJobsPerChat {
			slog.Warn("cron job over per-chat cap; skipping (raise cron.MaxJobsPerChat to restore)",
				"job_id", j.ID, "platform", j.Platform, "chat_id", j.ChatID, "cap", s.maxJobsPerChat)
			skippedOverPerChat++
			continue
		}
		if j.Paused {
			s.jobs[j.ID] = j
			s.addToChatIndexLocked(j)
			stubs = append(stubs, stubRow{j.ID, j.WorkDir, j.Prompt, j.LastSessionID})
			continue
		}
		if err := s.registerJob(j); err != nil {
			slog.Warn("skip invalid cron job", "job_id", j.ID, "schedule", j.Schedule, "err", err)
			continue
		}
		s.jobs[j.ID] = j
		s.addToChatIndexLocked(j)
		stubs = append(stubs, stubRow{j.ID, j.WorkDir, j.Prompt, j.LastSessionID})
	}
	jobCount := len(s.jobs)
	s.mu.Unlock()
	if skippedOverCap > 0 {
		slog.Warn("cron Start: jobs skipped due to maxJobs cap; remaining entries are still on disk",
			"skipped", skippedOverCap, "loaded", jobCount, "cap", s.maxJobs)
	}
	if skippedOverPerChat > 0 {
		slog.Warn("cron Start: jobs skipped due to per-chat cap; remaining entries are still on disk",
			"skipped", skippedOverPerChat, "loaded", jobCount, "cap", s.maxJobsPerChat)
	}
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
	if s.runStoreEnabled() {
		s.gcWG.Add(1)
		go func() {
			defer s.gcWG.Done()
			slog.Info("cron run history: cold-start GC starting")
			// 传 stopCtx 进 trimAll，Stop 可在 job 入口之间中断长时间的 GC 扫描 (#1019)。
			s.trimAllRuns(s.stopCtx, time.Now())
			slog.Info("cron run history: cold-start GC done")
		}()
	}
	// agentcore-cloud-sandbox §6.5: reconcile sandbox runs orphaned by the
	// previous process (pending files whose streams died with it). Async
	// like the GC pass above — each orphan costs a StopRuntimeSession
	// network call and must not block Start. gcWG-tracked so Stop() waits.
	s.gcWG.Add(1)
	go func() {
		defer s.gcWG.Done()
		s.reconcileSandboxPending()
	}()
	slog.Info("cron scheduler started", "jobs", jobCount)
	return nil
}
