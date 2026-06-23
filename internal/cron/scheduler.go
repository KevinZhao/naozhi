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
// Field-access discipline (mirrors the `// 读写:` annotation pattern from
// session/router_core.go, whose absence here was flagged by R250-ARCH-4
// / #1167): the fields below fall into four lifetime classes —
//   - Lifecycle: cron / stopCtx / stopCancel / started / stopped /
//     startedAtNanos / triggerWG / gcWG — written once at NewScheduler /
//     Start / Stop, otherwise read-only or atomic.
//   - Immutable-after-construction config: router / notifySender / agents /
//     agentCommands / location / notifyDefault / allowedRoot* /
//     jitterMax / slowThreshold / execTimeout / maxJobs / maxJobsPerChat
//     / storePath / stopBudget — set in NewScheduler, never reassigned, so
//     reads are lock-free.
//   - mu-guarded mutable state: jobs / chatJobCount / jobsByChat — all
//     reads and writes hold s.mu (RLock for reads, Lock for writes); the
//     three maps are mutated together so they never drift.
//   - Independently-synchronised state: runningJobs (sync.Map) /
//     telemetry (atomic.Pointer) / store* (storeMu / storeDirOnce /
//     saveSeq) / routerNilOnce — each carries its own primitive and does
//     NOT rely on s.mu.
//
// Per-field godoc below stays authoritative; this block is the index.
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
	// chatJobCount tracks the number of jobs per (Platform, ChatID) chat.
	// Maintained synchronously with s.jobs writes under s.mu so the
	// per-chat capacity check in addJobAcquiringLock is O(1) instead of
	// O(maxJobs). Entries are deleted when the count hits zero so the
	// map's working set tracks the live chat set rather than every chat
	// that has ever owned a job. R237-PERF-5 (#661).
	chatJobCount map[chatJobKey]int
	// jobsByChat is a per-(Platform, ChatID) index of *Job pointers.
	// Maintained synchronously with s.jobs writes under s.mu so
	// findByPrefixLocked iterates only the small slice of jobs in the
	// caller's chat instead of the full s.jobs map. With maxJobsHardCap=500
	// and a typical 1-5 jobs/chat, the prefix-match scan drops from O(500)
	// to O(5) per IM-prefix lookup (DeleteJob/PauseJob/ResumeJob via
	// withJobByPrefix). Entries are deleted when the slice empties so the
	// map's working set tracks the live chat set, mirroring chatJobCount.
	// (Platform, ChatID) of a job is immutable post-AddJob (UpdateJob
	// rejects both via JobUpdate field absence), so an entry never moves
	// across keys — add appends, delete swaps-and-shrinks. R242-GO-9 (#558).
	jobsByChat map[chatJobKey][]*Job
	// sortedJobIDs mirrors the keys of s.jobs in ascending ID order. It is
	// maintained incrementally (binary-search insert on add, binary-search
	// delete on remove) at the same s.mu-guarded seams that mutate s.jobs
	// (addToChatIndexLocked / deleteJobLocked), so the per-mutation persist
	// path no longer runs an O(N log N) slices.SortFunc inside the s.mu
	// critical section — it iterates this already-sorted slice instead.
	// R164029-PERF-9 (#1598).
	//
	// CORRECTNESS NOTE: s.jobs remains the single source of truth.
	// marshalJobsLocked treats sortedJobIDs as a hint: it validates that the
	// slice still matches s.jobs (same length, every ID present) and falls
	// back to building+sorting from the map if it drifted. Production
	// mutations all go through the two seams so the hint is always valid;
	// the fallback only fires for test helpers that poke s.jobs directly
	// (e.g. `s.jobs[id] = &Job{...}` without addToChatIndexLocked), which
	// must never silently drop a job from the on-disk snapshot.
	sortedJobIDs []string
	// router is set once in NewScheduler and never reassigned.
	router SessionRouter
	// notifySender / agents / agentCommands are populated from SchedulerConfig
	// at NewScheduler and treated as immutable thereafter — notifyTarget
	// reads notifySender without s.mu and executeOpt reads agents without
	// s.mu. A future caller must NOT mutate the maps in place; if
	// dynamic backend/agent registration ever lands, switch to
	// atomic.Pointer[map[...]] swap-on-write so reads stay lock-free without
	// racing the writer.
	//
	// #725: notifyTarget now resolves its send surface through the cron-local
	// NotifySender / PlatformReplier interfaces (notify_sender.go) instead of
	// indexing a map[string]platform.Platform and calling platform.SplitText /
	// platform.ReplyWithRetry directly. This severs the internal/cron →
	// internal/platform import edge (the wireup layer owns the adapter,
	// mirroring the cronSessionAdapter precedent R20260527122801-ARCH-1 /
	// #1318) and supersedes the deferred R219-ARCH-8 (#670) Notifier proposal:
	// the chunked send loop (SplitText → MaxReplyLength → per-chunk Reply),
	// the per-target retry budget, and the partial-delivery telemetry stay in
	// notifyTarget — only the concrete platform calls move behind the
	// interface, so per-chunk error reporting is unchanged.
	// R249-ARCH-27 (#991): notifySender / agents / agentCommands are bundled
	// behind a single atomic.Pointer[cronConfigMaps] instead of three bare
	// map fields. They are write-once at NewScheduler today and read
	// lock-free by notifyTarget / executeOpt; the prior shape leaned on a
	// documentation-only "do not mutate in place" contract that a future
	// dynamic-backend / hot-reload caller could silently break (data race
	// vs the lock-free readers). Wrapping the trio in one immutable struct
	// behind atomic.Pointer pre-pays that retrofit: a hot-reload writer
	// Store()s a freshly built *cronConfigMaps (copy-on-write) while readers
	// Load() a consistent snapshot of all three maps together — no torn read
	// across the platforms/agents/agentCommands boundary, no per-field lock.
	// Until that writer lands the pointer is set once and never swapped, so
	// the runtime cost is one extra pointer indirection on the read path.
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
	// stopBudget is the per-instance Stop() budget snapshotted from the
	// package-level default at NewScheduler. R249-CR-3 (#947): moving the
	// budget off a package-level var onto the Scheduler instance removes
	// the t.Parallel race where two tests overlapping a short-budget Stop
	// on different *Scheduler would clobber each other's budget swap.
	// Field is read on the Stop() goroutine only, after construction; no
	// atomic is needed (writers are confined to NewScheduler + the test
	// seam WithStopBudgetField, which the same test must serialise via
	// t.Cleanup).
	stopBudget time.Duration
	// watchdogInterruptTimeoutNanos is the per-instance interrupt-call timeout
	// (in nanoseconds) runDeadlineWatchdog bounds sess.InterruptViaControl at.
	// R20260607-GO-4 (#1904): moved off the package-level
	// watchdogInterruptTimeoutAtomic onto the Scheduler instance — mirroring
	// stopBudget (#947) — so two t.Parallel() timeout tests that each shorten
	// it on their own *Scheduler no longer clobber each other's override.
	// Atomic because the production read happens on the AfterFunc watchdog
	// goroutine concurrently with a test's per-instance Store. Initialised to
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
	// workDirCacheKeySuffix is "\x00" + allowedRoot + "\x00" +
	// allowedRootResolved precomputed at construction. allowedRoot and
	// allowedRootResolved are immutable after NewScheduler returns, so the
	// hot path in workDirResolveUnderRootCached only needs one
	// concatenation (workDir + suffix) instead of three per tick.
	// R20260526-PERF-002 (#1225).
	workDirCacheKeySuffix string
	// jitterMax is the scheduling jitter cap. See SchedulerConfig.JitterMax.
	// Immutable after NewScheduler returns, so no lock needed.
	jitterMax time.Duration
	// slowThreshold is the wall-clock budget beyond which a successful cron
	// execution is counted as "slow". See SchedulerConfig.SlowThreshold;
	// zero/negative reads fall through to defaultCronSlowThreshold at the
	// callsite. Immutable after NewScheduler returns. R241-ARCH-11 (#519).
	slowThreshold time.Duration
	// routerNilOnce ensures registerStubByValue logs the "router missing"
	// slog.Error at most once per Scheduler lifetime, so a router-less
	// test fixture (or a deployment that legitimately opted into
	// AllowNilRouter) does not spam the log across N ticks. Combined
	// with the boot-time slog.Error in NewScheduler this gives one loud
	// surface at startup and one loud surface at the first runtime
	// callsite — enough to flag the wireup bug without burying the
	// operator under repeated identical entries. R241-ARCH-6 (#510).
	routerNilOnce sync.Once
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
	// stopped 用 CAS 保证 Stop() 幂等。重复调用直接 return,避免:
	//   - 二次 NewTimer (gcTimer / deadline / triggerTimer) 浪费定时器槽位
	//   - 二次 persistJobsLocked + 落盘 (race 落盘文件)
	//   - 二次 cron.Stop() (robfig/cron 内部对此能容忍但成本是无意义的)
	// stopCancel 已经是 idempotent (sync.Once 内部保护),所以这里只需 CAS
	// 把 Stop 的整体 body 短路就够了。R20260526-GO-007。
	stopped atomic.Bool
	// stopCtx is the scheduler's lifecycle context. Storing context in a
	// struct is usually an anti-pattern, but here execute() is invoked via
	// a callback from robfig/cron whose signature has no ctx parameter, so
	// the scheduler itself owns the root context so Stop() can cancel in-
	// flight executions. Callers outside execute() take ctx as an argument.
	//
	// R249-ARCH-4 (#972) — ctx-as-arg exception is confined: real reads of
	// s.stopCtx are allowed ONLY on robfig/cron callback-derived paths
	// (scheduler_run.go execute/jitter/spawn, scheduler_notify.go reply
	// ctx, and the cold-start GC here) where no ctx parameter exists to
	// thread. TestStopCtx_ReadsConfinedToCallbackPaths enforces that an
	// unrelated method which already receives a ctx must NOT reach for this
	// field instead.
	//
	// R249-ARCH-8 (#974) — single authoritative cancel signal. There appear
	// to be two ways to cancel cron work (SchedulerConfig.ParentCtx being
	// cancelled by the host's shutdown, vs an explicit Stop() call), but they
	// are NOT independent cancel paths: stopCtx is derived from ParentCtx via
	// context.WithCancel (NewScheduler line ~983), so a ParentCtx cancel
	// propagates INTO stopCtx, and Stop() cancels stopCtx directly via
	// stopCancel. Every in-flight read (execute()/trimAllCtx/notifyTarget)
	// observes exactly one signal — stopCtx.Done() — regardless of which
	// upstream fired it. stopCtx is therefore the authoritative signal;
	// ParentCtx is only a derive-time parent and must never be read for
	// cancellation after NewScheduler returns.
	stopCtx    context.Context
	stopCancel context.CancelFunc
	// telemetry receives the cron-run lifecycle events. Phase D (RFC §3.5)
	// replaced the legacy onExecute / onRunStarted / onRunEnded
	// atomic.Pointer trio with a single broadcaster; R20260527-GO-1
	// reverted the storage to atomic.Pointer because SetTelemetry can
	// land after tick goroutines have already started (cmd/naozhi
	// orchestration is not strictly boot-only) — emitRunStarted /
	// emitRunEnded read this field on the cron-dispatch path while
	// SetTelemetry could be writing it from the wiring goroutine.
	//
	// Broadcaster is an interface, so we store *Broadcaster (atomic
	// pointer to the interface value) and Load + deref before use.
	// nil pointer == no broadcaster (tests / no-WS setups).
	telemetry atomic.Pointer[runtelemetry.Broadcaster]

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

	// jobGates shards a fixed pool of mutexes (indexed by hashed jobID) that
	// serialise executeOpt's jobInflight-load→CAS pair against
	// cleanupRunningJobIfIdle's load→CompareAndDelete pair for the same job.
	// Closes the TOCTOU window where a DeleteJob racing TriggerNow orphans the
	// CAS gate and permits double execution. See jobGateLock (job_gate.go) for
	// the full rationale. R20260603140013-GO-2 (#1706).
	jobGates [jobGateShards]sync.Mutex

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

	// sandboxPendingMu guards sandboxPendingIndex. It is independent of s.mu
	// (the §6.5 write/remove seams run lock-free w.r.t. the jobs trio) so the
	// hot delete path never contends with job CRUD. RWMutex so the pure-read
	// lookup path (lookupSandboxPendingIndex, the hot delete fast path) does not
	// serialize against concurrent reads [R202606-PERF-001].
	sandboxPendingMu sync.RWMutex
	// sandboxPendingIndex maps jobID → its in-flight §6.5 pending file path,
	// maintained write-authoritatively at the two write paths
	// (writeSandboxPending sets, the terminal remove clears). It lets
	// stopSandboxRunsForJob resolve a deleted job's pending file with one map
	// lookup instead of os.ReadDir + per-file ReadFile/unmarshal over EVERY
	// concurrent run's record (R20260616-PERF-001 / #2140). Only tracks records
	// THIS process wrote; orphans inherited from a previous boot are handled by
	// reconcileSandboxPending (which still scans the dir, by design). The per-
	// job CAS guarantees at most one in-flight run per job, so a single path
	// value per key suffices.
	sandboxPendingIndex map[string]string

	// workDirCache memoises positive workDirResolveUnderRoot results so
	// fast-firing jobs do not repeat the EvalSymlinks chain (~Lstat+Readlink
	// per path component) every tick. TTL-bounded
	// (workDirResolveCacheTTL) so symlink retargets surface within one
	// notify-budget worth of time. R247-PERF-24.
	workDirCache workDirResolveCache

	// workDirReachableCache memoises positive workDirReachable() results so
	// fresh-mode jobs whose allowedRoot=="" (and thus never touch
	// workDirCache) do not issue a bare os.Stat every tick. Same 30s TTL
	// and positive-only semantics as workDirCache: a negative (unreachable)
	// result bypasses the cache so a restored workspace surfaces on the
	// next tick. Keyed by raw workDir. R20260604064416-PERF-3 (#1731).
	workDirReachableCache workDirResolveCache

	// knownSessionsCache memoises KnownSessionIDs() output for up to
	// knownSessionsCacheTTL. The dashboard polls KnownSessionIDs at 1Hz
	// per tab; rebuilding the set walks every job's runStore.Recent (up
	// to ~jobs × 200 file metadata reads) per call. The TTL cache cuts
	// that to one rebuild per 30s. Invalidated explicitly on writes that
	// can change the set (LastSessionID assignment, runStore.Append).
	// R250-PERF-7.
	knownSessionsCache knownSessionsCache

	// marshalJobs is the JSON serializer used by marshalJobsLocked, held
	// behind atomic.Pointer so tests (withFailingMarshal in
	// persist_failure_test.go) can swap a failing stub without racing
	// concurrent persist hot-path readers under -race. Initialised to
	// defaultMarshalJobs in NewScheduler. Lifted from a package-level
	// var so the seam stops being a global and parallel tests can no
	// longer leak a stub across schedulers. R250-ARCH-14 closes
	// R242-CR-5 (#693) / R246-ARCH-18 / R247-CR-19 (#599) — the older
	// anchors all describe the same package-level mutable seam this
	// per-Scheduler atomic.Pointer field replaced; pinned by
	// TestMarshalJobs_PerSchedulerIsolation in
	// marshal_jobs_per_scheduler_test.go.
	marshalJobs atomic.Pointer[marshalJobsFn]

	// clock is the time source for lifecycle timestamps (run finish endedAt,
	// synthetic-skipped startedAt). Defaults to realClock (time.Now) in
	// NewScheduler; tests inject a fake to pin a deterministic now without
	// sleeping. R247-ARCH-11 / R245-ARCH-34 (#643) — minimal injection point;
	// see clock.go. Read via s.now() so a zero-value Scheduler still falls back
	// to wall-clock time rather than nil-panicking.
	clock cronClock
}

// NewScheduler creates a scheduler. Call Start() to begin. cfg carries the
// value/scalar configuration; deps carries the injected components (cfg/deps
// split per RFC cron-sysession-merge §3.5.1, #746 — see deps.go).
func NewScheduler(cfg SchedulerConfig, deps SchedulerDeps) *Scheduler {
	// R20260526-GO-023 / R241-ARCH-6 (#510): surface missing router wiring
	// at construction so the misconfiguration shows up at boot rather than
	// as an opaque NPE stack trace from executeOpt the first time a job
	// ticks. We log slog.Error (not panic) because the test suite
	// (persist_failure_test, scheduler_test, stop_budget_test,
	// trigger_now_wg_done_test, …) constructs Schedulers without a router
	// for narrowly-scoped paths (AddJob validation, persist failure
	// injection, stop-budget timing) that never reach executeOpt;
	// panicking would force a sprawling rewrite across dozens of unrelated
	// test files. The companion R20260526-GO-004 guard inside executeOpt
	// then short-circuits the hot path so a router-less fixture does not
	// NPE if a job somehow does tick.
	//
	// R241-ARCH-6 (#510): Tests that legitimately want silence opt in via
	// SchedulerConfig.AllowNilRouter (additive, default false). Production
	// wiring (cmd/naozhi) sets Router to a non-nil adapter so the error
	// fires only for misconfigurations + tests that haven't opted in. The
	// upgrade from slog.Warn to slog.Error matches the operator
	// expectation: a missing router means the dashboard sidebar is empty
	// for the lifetime of the process — that's a wireup bug, not a
	// transient warning.
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
	// R241-ARCH-10 (#517): NUL-byte sanitisation + EvalSymlinks resolution
	// of AllowedRoot is now folded behind one helper so NewScheduler reads
	// as a flat field-mirror. The helper mutates cfg.AllowedRoot in place
	// (clearing it on NUL detection) and returns the symlink-resolved
	// path. Kept separate from applyDefaults because EvalSymlinks is a
	// syscall and applyDefaults is documented as pure / idempotent.
	allowedRootResolved := cfg.resolveAllowedRoot()
	cronLogger := robfigcron.PrintfLogger(slogPrintfLogger{})
	// applyDefaults guarantees Location is non-nil (defaults to time.Local).
	loc := cfg.Location
	// R232-CR-4: surface "general" fallback being absent. ResolveAgent returns
	// "general" when the prompt has no slash-prefix; if that agent isn't
	// configured, executeOpt reads a zero AgentOpts (empty Backend / Model
	// / Workspace) and the cron tick spawns with backend defaults
	// silently. Logging at construction makes the misconfiguration visible
	// without changing runtime behaviour.
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
		// R241-ARCH-3 (#506) + R249-ARCH-27 (#991): notifySender / agents /
		// agentCommands are documented as immutable after NewScheduler so
		// notifyTarget + executeOpt can read them lock-free. The constructor
		// previously aliased the caller-supplied maps verbatim — leaving the
		// immutability contract dependent on the caller's discipline. A
		// late-binding wireup that re-assigned deps.Agents[name]
		// (legitimate at boot, dangerous post-Start) would race the lock-free
		// reads in cron-package hot paths. maps.Clone severs the alias at
		// construction so the contract is enforced by the receiver, not by
		// caller trust; the cloned maps + the (interface-valued) NotifySender
		// are then published as one immutable *cronConfigMaps via
		// configMapsPtr.Store below so readers Load() a consistent cross-field
		// snapshot. maps.Clone returns nil for nil input so callers that omit
		// any map field (test fixtures, narrow integration tests) keep the
		// prior nil-map semantics — indexing a nil map is a safe zero-value
		// read. #725: NotifySender carries no backing array, so it is stored
		// directly (no clone).
		storePath:      cfg.StorePath,
		maxJobs:        cfg.MaxJobs,
		maxJobsPerChat: maxPerChat,
		execTimeout:    cfg.ExecTimeout,
		sandbox:        deps.Sandbox,
		// R249-CR-3 (#947): seed the per-instance field so tests in
		// t.Parallel can override their own Scheduler's budget without
		// racing each other on a global.
		// R20260603150052-GO-2 (#1712): seed directly from the
		// defaultStopBudget const instead of a package-level var. Tests
		// inject a short budget via WithStopBudgetField on the instance,
		// so the global var (and the Stop() fallback that read it) is gone.
		stopBudget:          defaultStopBudget,
		location:            loc,
		notifyDefault:       cfg.NotifyDefault,
		allowedRoot:         cfg.AllowedRoot,
		allowedRootResolved: allowedRootResolved,
		// R20260526-PERF-002 (#1225): precompute the cache-key suffix so
		// the hot path in workDirResolveUnderRootCached avoids three
		// per-tick string concats. Must stay byte-for-byte identical to
		// workDirResolveCacheKey(workDir, allowedRoot, allowedRootResolved)
		// minus the leading workDir, otherwise cache lookups miss.
		workDirCacheKeySuffix: "\x00" + cfg.AllowedRoot + "\x00" + allowedRootResolved,
		jitterMax:             cfg.JitterMax,
		slowThreshold:         cfg.SlowThreshold,
		stopCtx:               stopCtx,
		stopCancel:            stopCancel,
		runStore:              newRunStore(cfg.StorePath, cfg.RunsKeepCount, cfg.RunsKeepWindow),
		sandboxPendingIndex:   make(map[string]string),
		// R247-ARCH-11 (#643): install the real-time clock by default. Tests
		// swap a fake via the withClock seam to pin lifecycle timestamps
		// (run DurationMS, skipped-run startedAt) without sleeping.
		clock: defaultClock,
	}
	// R250-ARCH-14: initialise the per-Scheduler marshal seam so the
	// hot path in marshalJobsLocked finds defaultMarshalJobs instead of
	// nil. Tests swap a failing stub via withFailingMarshal.
	s.marshalJobs.Store(&defaultMarshalJobs)
	// R20260607-GO-4 (#1904): seed the per-instance watchdog interrupt timeout
	// from the package-level default. Tests override it per-instance via
	// setWatchdogInterruptTimeoutForScheduler, so no global mutation races.
	s.watchdogInterruptTimeoutNanos.Store(int64(watchdogInterruptTimeoutDefault))
	// R249-ARCH-27 (#991): publish the cloned config maps as one immutable
	// snapshot. maps.Clone severs the alias to the caller-supplied maps so
	// post-Start mutation of cfg.* cannot race the lock-free readers; the
	// atomic.Pointer makes a future hot-reload swap race-free without
	// retrofitting every read site. Set once here, never swapped today.
	s.configMapsPtr.Store(&cronConfigMaps{
		// #725: NotifySender is an interface value, not a map — write it once
		// into the snapshot (no maps.Clone: there is no backing array to alias
		// away from a late-binding wireup writer).
		notifySender:  deps.NotifySender,
		agents:        maps.Clone(deps.Agents),
		agentCommands: maps.Clone(deps.AgentCommands),
	})
	// R20260527-GO-1: install the broadcaster via atomic.Pointer so
	// later SetTelemetry calls are race-free vs the cron-dispatch read
	// path in emitRunStarted / emitRunEnded. nil deps.Telemetry leaves
	// the pointer as zero-value (no broadcast).
	if deps.Telemetry != nil {
		b := deps.Telemetry
		s.telemetry.Store(&b)
	}
	// R238-SEC-12 (#834): close the startup permission window. The
	// storeDirOnce gate in saveMarshaledSeq only fires on the *first*
	// save, so between process start and that first mutation the parent
	// data dir keeps whatever mode it inherited from XDG (often 0o755) —
	// a local attacker could enumerate cron_jobs.json's existence /
	// mtime in that window. Run the same MkdirAll(0o700) eagerly at
	// construction; once.Do later in saveMarshaledSeq becomes a no-op,
	// so the hot-path cost is unchanged.
	//
	// R238-SEC-10 (#830): MkdirAll only sets permissions when CREATING
	// the directory — if the operator pre-created the parent data dir
	// (or it inherited XDG_CONFIG_HOME at 0o755), MkdirAll is a no-op
	// and the broader perms persist. Defense-in-depth: Chmod(0o700)
	// after MkdirAll so the parent directory is always clamped to
	// owner-only. Both `cron_jobs.json` (0o600) and `runs/` (0o700) are
	// already restrictive; tightening the parent prevents other local
	// users from listing the cron data dir's contents (e.g. confirming
	// cron_jobs.json's existence and mtime). Failures are logged and
	// non-fatal — perms may already be tighter, or we may not own the
	// dir; in either case the per-file modes are still the strict
	// barrier.
	if cfg.StorePath != "" {
		s.storeDirOnce.Do(func() {
			if dir := filepath.Dir(cfg.StorePath); dir != "" && dir != "." {
				// R040034-GO-9 (#1395): bump MkdirAll failure severity
				// from Warn → Error. NewScheduler can't return an error
				// (no breaking the constructor signature), but a failed
				// MkdirAll guarantees the next saveMarshaledSeq will
				// ENOENT on WriteFileAtomic — operators previously saw
				// a runtime "save cron store" error several minutes
				// after boot rather than a clean boot-time signal. The
				// Chmod fail-on-existing path stays Warn because a
				// pre-existing dir at the wrong perms is recoverable
				// (operator chmod) without losing persistence.
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
// can show users where a "notify on completion" toggle will deliver
// messages. The value is the snapshot captured at NewScheduler time from
// SchedulerConfig.NotifyDefault — runtime mutation is not supported, so
// callers can cache the return value for the lifetime of the process.
//
// Returns the zero NotifyTarget when no fallback was configured; the
// dashboard uses NotifyTarget.IsSet() to decide whether to render the
// toggle hint. The zero value is also what jobs without an explicit
// Notify target fall back to inside resolveNotifyTarget.
//
// Safe to call on a nil *Scheduler: returns the zero NotifyTarget. This
// matches the nil-safe pattern used by Location() / StartedAt() so the
// dashboard can render a placeholder during the bootstrap window before
// the scheduler is wired. R247-CR-9.
func (s *Scheduler) NotifyDefault() NotifyTarget {
	if s == nil {
		return NotifyTarget{}
	}
	return s.notifyDefault
}

// StartedAt 返回 Scheduler 最近一次 Start() 的时刻。用于 missed-schedule
// 检测的启动抑制窗口。未 Start 前返回零值。
//
// Safe to call on a nil *Scheduler: returns the zero time. R249-CR-11
// (#955): NotifyDefault()'s godoc already advertised StartedAt() as part
// of the nil-safe read-accessor family used by the dashboard during the
// bootstrap window, but the method dereferenced s.startedAtNanos with no
// nil guard — a documented-but-unenforced contract. The dashboard reads
// these three accessors (Location / NotifyDefault / StartedAt) together to
// render a placeholder before the scheduler is wired, so a nil receiver on
// one of them must not panic while the other two return safely.
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
//
// R250-ARCH-5 (#1168): this is the idiomatic Go entry point (Start(ctx) /
// Stop(ctx)) that callers should prefer over stashing a lifecycle ctx in
// SchedulerConfig.ParentCtx. The ParentCtx field remains supported for
// back-compat, but a non-nil ctx passed here is wired so that its
// cancellation propagates INTO stopCtx exactly like ParentCtx does — when
// ctx is cancelled (app shutdown), running cron jobs are interrupted via the
// same stopCtx.Done() signal Stop() drives. This avoids forcing operators to
// construct a full SchedulerConfig literal just to thread the app ctx and
// keeps cron's startup contract off the ctx-on-struct anti-pattern
// (golang/go#36363).
//
// Semantics:
//   - A nil ctx is treated as "no extra parent" and behaves identically to
//     calling Start() directly.
//   - The watcher goroutine exits when EITHER ctx or stopCtx is done, so it
//     never outlives the scheduler (Stop() cancels stopCtx and unblocks it).
//   - StartContext is safe to call instead of Start; it is NOT meant to be
//     combined with a separate Start() call. Like Start() it is idempotent
//     via the same started/stopped CAS guards.
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
// Idempotency (R241-ARCH-2): a second Start() returns nil immediately
// without re-loading jobs, re-spawning the cold-start GC pass, or
// re-invoking robfig/cron's Start (which would panic on a running
// runner). The CAS guard runs *before* startedAtNanos.Store so a
// double-Start does not reshape the missed-schedule suppression
// window mid-flight.
func (s *Scheduler) Start() error {
	// R249-ARCH-19 (#984): refuse to (re)start once Stop() has latched. The
	// Stop() godoc documents that triggerWG / gcWG / runDeadlineWatchdog
	// wrapper goroutines are intentionally leaked on budget-exceed precisely
	// because "Scheduler is not reusable". A Stop-then-Start sequence would
	// re-enter loadJobs + cron.Start + cold-start GC on an instance whose
	// stopCtx is already cancelled, accumulating those orphan goroutines
	// across lifecycles until OOM. The started CAS below blocks the common
	// double-Start, but it does NOT block Start-after-Stop when a prior
	// Start failed at loadJobs and reset started=false. Gate on stopped here
	// so a Stopped instance can never be revived. ErrSchedulerStopped lets
	// callers distinguish "already stopped" from a transient load failure.
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
		// R241-ARCH-2: on load failure, release the idempotency latch so
		// the operator can retry Start() after fixing the store file.
		// R246-CR-246: also reset startedAtNanos. The original comment
		// above the Store claimed "下次重试会覆盖" but that only holds if
		// the next Start() actually succeeds quickly. If retry is delayed,
		// the stale startedAtNanos timestamp would feed HasMissedSchedule's
		// startup-suppression window (currently NOT yet running because
		// started=false but exposed via StartedAt() to callers like
		// dashboard / metrics) and report a false "running since N min ago"
		// state. Clearing it keeps StartedAt() == zero until a Start() that
		// actually hands off to cron runner.
		// R20260607-CORR-1: clear nanos BEFORE clearing started so a concurrent
		// retry goroutine that wins the CAS after started=false cannot observe
		// started=true with a stale (non-zero) startedAtNanos value.
		s.startedAtNanos.Store(0)
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
	// R250-ARCH-26 (#1187): enforce s.maxJobs cap during restore so an
	// operator who lowered MaxJobs in config.yaml after the on-disk store
	// already exceeded the new cap doesn't silently load every persisted
	// job. addJobAcquiringLock is the only other code path that checks the
	// cap; without this gate the runtime cap was advisory at best (the next
	// AddJob would fail at len(s.jobs)+1 > maxJobs but the over-cap entries
	// already loaded would keep running). Skipped jobs stay on disk so an
	// operator can recover them by raising the cap and restarting; they are
	// NOT removed from cron_jobs.json by the next persist (Start does not
	// trigger a save unless a CRUD mutation lands). A WARN per skipped job
	// names the ID + schedule so the operator sees which jobs were dropped.
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
		// R250-ARCH-26 (#1187): refuse to register beyond s.maxJobs. The
		// check fires AFTER the workDir filter so the operator's quota
		// counts only sandbox-valid jobs (a sandbox-rejected entry should
		// not consume a cap slot).
		if len(s.jobs) >= s.maxJobs {
			slog.Warn("cron job over maxJobs cap; skipping (raise cron.MaxJobs to restore)",
				"job_id", j.ID, "schedule", j.Schedule, "cap", s.maxJobs)
			skippedOverCap++
			continue
		}
		// R20260613-CR-10 (#2060): enforce the per-chat cap on the startup
		// load path too. AddJob (scheduler_jobs.go) rejects beyond
		// maxJobsPerChat, but loadJobs went straight to addToChatIndexLocked,
		// so a legacy / hand-edited cron_jobs.json with an over-cap chat would
		// load all entries and leave the in-memory chatJobCount above the cap —
		// then AddJob reports "per-chat limit reached" while the operator
		// believes there is headroom. Clamp here so the loaded count matches
		// the cap semantics; over-cap entries stay on disk (recoverable by
		// raising the cap + restart), mirroring the maxJobs skip above.
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
			// R234-GO-3 / #1019: 传 stopCtx 进 trimAll，Stop 可在 job 入口
			// 之间中断长时间的 GC 扫描，避免 Stop 等到 gcWaitBudget。
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
