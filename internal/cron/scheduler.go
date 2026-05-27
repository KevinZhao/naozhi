package cron

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	robfigcron "github.com/robfig/cron/v3"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/runtelemetry"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// ErrJobNotFound is returned by lookup/mutation APIs when no cron job matches.
// Callers should use errors.Is(err, cron.ErrJobNotFound) instead of string matching.
var ErrJobNotFound = errors.New("cron: job not found")

// ErrAmbiguousPrefix is returned by findByPrefixLocked when an ID prefix matches more
// than one job in the same chat scope. Callers (CLI/HTTP) should use
// errors.Is(err, cron.ErrAmbiguousPrefix) to surface a "please disambiguate"
// hint instead of treating it as a generic not-found. [R247-GO-2]
var ErrAmbiguousPrefix = errors.New("cron: ambiguous job prefix")

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
	//
	// R242-ARCH-26 (#768): from cron's side every caller (registerStubByValue
	// at scheduler.go) builds at most a 1-element slice — `[]string{lastSessionID}`
	// when lastSessionID != "" else nil. The slice signature is preserved for two
	// reasons:
	//
	//  1. session.Router (the implementer) consumes chainIDs as a generic
	//     prevSessionIDs link to support non-cron callers in the future
	//     (e.g. workspace-chain bind on user sessions). Collapsing the cron
	//     interface to (chainID string) would force a parallel signature on
	//     the session side or a slice rebuild at every cross-package call —
	//     net zero ergonomics improvement, plus a wider API surface.
	//  2. The empty / nil branch is a meaningful "no chain" signal that a
	//     scalar string would have to overload onto "" (which is also
	//     ambiguous with "lastSessionID was set but is empty" — a state we
	//     never want to enter, but couldn't structurally rule out).
	//
	// If a future cron caller legitimately needs a multi-step chain (e.g.
	// daisy-chained fresh_context resets that want to preserve N>1 generations
	// of JSONL history), the slice already supports it — no signature change.
	RegisterCronStubWithChain(key, workspace, lastPrompt string, chainIDs []string)
	// Reset discards the session for the given key (used by fresh-mode
	// cron jobs and by Delete/Rename flows).
	//
	// Concurrency contract (R249-CR-14):
	//   - Reset MUST NOT block on in-flight turns. Implementations must
	//     short-circuit (or asynchronously complete) any active Send so
	//     callers — notably the cron run goroutine that calls Reset
	//     between two ticks — see bounded latency. A blocking Reset
	//     would let one slow CLI turn pin the entire scheduler tick
	//     loop and starve subsequent jobs.
	//   - Callers MUST NOT hold scheduler.mu (or any lock the router's
	//     notifyChange callback might re-acquire) when invoking Reset.
	//     The router's Reset path may synchronously fan out a
	//     notifyChange that re-enters scheduler state (e.g. to refresh
	//     the dashboard projections), and re-entrant lock acquisition
	//     would deadlock. Reset is invoked only from execute-time call
	//     sites that have already released scheduler.mu.
	Reset(key string)
	// GetOrCreate returns an existing session or spawns a new one at
	// execute time. Returns cron-local Session / SessionStatus types so
	// the scheduler does not transitively depend on internal/session.
	// The production wireup (cmd/naozhi/cron_router_adapter.go) wraps
	// *session.ManagedSession in a cron.Session adapter.
	GetOrCreate(ctx context.Context, key string, opts AgentOpts) (Session, SessionStatus, error)
}

// SchedulerConfig holds configuration for the cron scheduler.
type SchedulerConfig struct {
	// Router is the session router the scheduler talks to. Accepts the
	// SessionRouter interface so tests can pass a minimal fake; production
	// passes a *session.Router which satisfies it transparently.
	Router        SessionRouter
	Platforms     map[string]platform.Platform
	Agents        map[string]AgentOpts
	AgentCommands map[string]string
	// Telemetry receives RunStartedEvent / RunEndedEvent for every cron
	// run via the shared runtelemetry shape. nil = no broadcast (tests /
	// no-WS deployments). Replaces the legacy SetOnRunStarted /
	// SetOnRunEnded / SetOnExecute setter trio. Late injection is also
	// supported via SetTelemetry — cmd/naozhi builds the Scheduler before
	// the Hub exists, then wires the broadcaster from dashboard.go. (RFC §3.5)
	Telemetry runtelemetry.Broadcaster
	StorePath string
	MaxJobs   int
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
	//
	// R245-GO-6 (#846): NewScheduler reads ParentCtx once and passes it to
	// context.WithCancel; the SchedulerConfig itself is taken by value and
	// not retained on *Scheduler. The only long-lived reference is the
	// derived stopCtx (held internally and cancelled by Stop()). Callers
	// can therefore set ParentCtx to a request-scoped or test-scoped ctx
	// without leaking the parent's value-tree past the Scheduler's
	// lifetime — the parent is "derived-only" from the Scheduler's
	// perspective and the caller-supplied ctx may be discarded
	// immediately after NewScheduler returns.
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
	// RunsKeepCount overrides DefaultRunsKeepCount when > 0. Sets the
	// per-job cap on retained run-history records (newest N kept). Zero
	// (and negative) values fall back to the default — additive: existing
	// callers that omit the field keep the prior behaviour.
	RunsKeepCount int
	// RunsKeepWindow overrides DefaultRunsKeepWindow when > 0. Records
	// older than the window are trimmed at GC time. Zero (and negative)
	// values fall back to the default; additive (callers that omit it
	// keep prior behaviour). R250-GO-3.
	RunsKeepWindow time.Duration
	// SlowThreshold overrides defaultCronSlowThreshold (30s) when > 0.
	// A successful cron execution exceeding this wall-clock budget is
	// counted as "slow" (metrics.CronExecutionSlowTotal +
	// "cron execution slow" warn). 30s suits typical interactive-agent
	// jobs but flags every long batch run when ExecTimeout is set to
	// 300s+; raising SlowThreshold to align with ExecTimeout silences
	// the daily false-alarm without losing the metric for jobs that
	// truly tip over the operator's expectation. Zero (and negative)
	// values fall back to defaultCronSlowThreshold so callers that omit
	// the field keep the prior behaviour. R241-ARCH-11 (#519).
	SlowThreshold time.Duration
	// AllowNilRouter opts the constructor out of the boot-time
	// "router required" slog.Error contract added in R241-ARCH-6 (#510).
	// Production wiring (cmd/naozhi) always sets Router; this flag exists
	// so the in-package test suite — which constructs Schedulers for
	// narrowly scoped paths (AddJob validation, persist failure injection,
	// stop-budget timing) that never reach executeOpt or registerStub —
	// can opt into silence without coupling those tests to a fakeRouter
	// dependency. When unset (the default) and Router is nil,
	// NewScheduler emits a slog.Error so misconfigurations surface at
	// boot rather than as an opaque empty-sidebar at runtime; the
	// registerStubByValue sync.Once log adds a second loud signal the
	// first time the missing router would have refreshed a stub.
	AllowNilRouter bool
}

// chatJobKey identifies a (Platform, ChatID) pair for the per-chat job
// counter. R237-PERF-5 (#661): replaces the O(N) scan over s.jobs that
// addJobAcquiringLock used to enforce maxJobsPerChat. The scan held s.mu
// across maxJobs entries on every AddJob — a direct hot-path block on
// the dashboard 1Hz add path. With this counter map the per-chat
// capacity check is one map lookup. Updates piggy-back on the already-
// locked s.mu sections in addJobAcquiringLock / deleteJobLocked / Start
// so the counter never drifts from len-by-chat(s.jobs).
type chatJobKey struct {
	Platform string
	ChatID   string
}

// Scheduler manages cron jobs and executes them on schedule.
type Scheduler struct {
	cron *robfigcron.Cron
	mu   sync.RWMutex
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
	agents        map[string]AgentOpts
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

	// workDirCache memoises positive workDirResolveUnderRoot results so
	// fast-firing jobs do not repeat the EvalSymlinks chain (~Lstat+Readlink
	// per path component) every tick. TTL-bounded
	// (workDirResolveCacheTTL) so symlink retargets surface within one
	// notify-budget worth of time. R247-PERF-24.
	workDirCache workDirResolveCache

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
	// longer leak a stub across schedulers. R250-ARCH-14.
	marshalJobs atomic.Pointer[marshalJobsFn]
}

// knownSessionsCache holds a TTL-bounded snapshot of KnownSessionIDs
// output. Set is read-only after publication so callers can hand out
// the map directly without copying. R250-PERF-7.
type knownSessionsCache struct {
	mu          sync.Mutex
	generatedAt time.Time
	set         map[string]struct{}
}

// knownSessionsCacheTTL bounds how stale a cached KnownSessionIDs
// snapshot may be. 30s matches the godoc claim and is well below the
// auto-workspace-chain spawn cadence (one spawn per user message);
// dashboard 1Hz pollers see at most one rebuild per cache cycle. R250-PERF-7.
const knownSessionsCacheTTL = 30 * time.Second

// maxJobsHardCap caps user-configurable MaxJobs to prevent accidental
// overload. 500 jobs ≈ 500 tick timers; well within robfig/cron's tested
// scale, but higher values tend to indicate a config mistake.
// See docs/rfc/cron-v2-polish.md for sizing rationale.
const maxJobsHardCap = 500

// defaultMaxJobs is the fallback for SchedulerConfig.MaxJobs when the operator
// leaves it zero/negative. Sized for typical single-tenant deployments; the
// hard cap above protects against runaway configs.
// See docs/rfc/cron-v2-polish.md for sizing rationale.
const defaultMaxJobs = 50

// defaultExecTimeout bounds a single job execution when the operator leaves
// SchedulerConfig.ExecTimeout zero. 5 min covers nearly all CLI turn budgets
// without leaving runaway jobs holding the per-job overlap gate forever.
// See docs/rfc/cron-v2-polish.md for sizing rationale.
const defaultExecTimeout = 5 * time.Minute

// DefaultMaxJobsPerChat bounds how many cron jobs a single chat (platform+
// chat_id pair) may own. Prevents one loud group from consuming the
// global MaxJobs quota. Exported so tests and docs can reference the
// value; operators can override per deployment via
// SchedulerConfig.MaxJobsPerChat (zero / unset falls back to this
// default — no way to "disable" the cap without rebuilding).
//
// Relationship to exempt pool:
// Every cron job calls session.Router.RegisterCronStubWithChain at scheduler
// Start / AddJob time and consumes 1 slot from session.maxCronExempt — a
// dedicated cron-only sub-quota inside the global maxExemptSessions pool
// (R242-ARCH-2). Planner and sys daemon stubs have their own sub-quotas
// (maxProjectExempt / maxSysExempt) and can no longer be starved by a
// noisy cron chat. DefaultMaxJobsPerChat still bounds per-chat usage so
// one loud group cannot saturate the cron quota by itself.
// See docs/rfc/cron-v2-polish.md for sizing rationale.
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
	_, ok := workDirResolveUnderRoot(workDir, allowedRoot, allowedRootResolved)
	return ok
}

// workDirResolveCacheTTL caps how long a positive workDirResolveUnderRoot
// result may be reused before re-running EvalSymlinks. R247-PERF-24:
// long-lived schedulers re-evaluate the same per-job workDir every tick;
// each call costs Lstat+Readlink per path component plus the same chain
// for allowedRoot. A short TTL collapses the hot-path syscall load on
// fast-firing jobs while still bounding the TOCTOU window the per-call
// EvalSymlinks was added to close. 30s matches the cronNotifyTimeout
// budget — an operator who retargets a workspace symlink and immediately
// fires a job will see the next tick re-resolve, not the same tick. Only
// "ok" results are cached: a negative answer means we just refused to
// run, and we want a re-resolve on the next call to surface a workspace
// that has been restored.
const workDirResolveCacheTTL = 30 * time.Second

// workDirResolveCacheMaxEntries caps the workDirResolveCache so a buggy
// or hostile job-creation flow that varies WorkDir per call (each call
// produces a distinct map key) cannot grow the cache without bound.
// R20260527-SEC-4 (#1273). The cap is generous: real deployments see
// at most one entry per cron job (≤200 typical), so 4096 leaves ample
// headroom for legitimate distinct workspaces while bounding worst-case
// memory at ~1 MB (key+value ≈ 256 B). When `store` observes the size
// at-or-above the cap it triggers a one-shot sweep that drops every
// expired entry; if still at-or-above after the sweep it returns
// without inserting. Skipping the insert is correctness-safe — the
// next call simply pays the EvalSymlinks cost again, exactly as it
// would on a cold boot — and prevents pathological growth.
const workDirResolveCacheMaxEntries = 4096

// workDirResolveCacheEntry captures one cached resolution. Stored value-
// typed in sync.Map so the read path does no allocation.
type workDirResolveCacheEntry struct {
	resolved  string
	expiresAt time.Time
}

// workDirResolveCache memoises positive workDirResolveUnderRoot results
// keyed by raw (workDir,allowedRoot,allowedRootResolved) tuple. Negative
// results bypass the cache. Concurrent-safe via sync.Map; entries expire
// lazily on read so a wedged job does not pin stale resolutions
// indefinitely. R247-PERF-24.
//
// Entry-count bound (R20260527-SEC-4 / #1273): sync.Map exposes no Len,
// so we maintain `size` via atomic add on store/Delete and use it as a
// soft cap (workDirResolveCacheMaxEntries). On crossing the cap a
// `sweep` walks every entry and Delete()s expired ones; if the cap is
// still exceeded the new entry is dropped. The cap is generous so
// healthy workloads never trigger sweep — only a misbehaving caller
// that varies WorkDir per call (e.g. random suffix) does. Sweep is
// O(N) but bounded by the cap and only fires at the cap boundary.
type workDirResolveCache struct {
	m    sync.Map     // map[string]workDirResolveCacheEntry
	size atomic.Int64 // approximate live entry count; never goes negative
}

// nowFn is overridable for tests so the TTL boundary can be exercised
// deterministically. Production always reads time.Now.
func (c *workDirResolveCache) lookup(key string, now time.Time) (string, bool) {
	if c == nil {
		return "", false
	}
	v, ok := c.m.Load(key)
	if !ok {
		return "", false
	}
	e := v.(workDirResolveCacheEntry)
	if !now.Before(e.expiresAt) {
		// Expired — drop so the next miss path doesn't keep observing it.
		// LoadAndDelete makes the size decrement race-safe: if a
		// concurrent goroutine already removed the entry we don't double-
		// count the decrement.
		if _, loaded := c.m.LoadAndDelete(key); loaded {
			c.size.Add(-1)
		}
		return "", false
	}
	return e.resolved, true
}

// sweep walks the map and Delete()s every entry whose expiresAt is on or
// before now, decrementing size for each. R20260527-SEC-4 (#1273): the
// cap-trigger path needs a way to actively prune so a sustained burst
// of distinct keys cannot pin the cap forever (lookup-driven lazy
// expiry only fires on re-query, which a one-shot key never sees).
// Walk is O(N) but bounded by workDirResolveCacheMaxEntries.
func (c *workDirResolveCache) sweep(now time.Time) {
	if c == nil {
		return
	}
	c.m.Range(func(k, v any) bool {
		e := v.(workDirResolveCacheEntry)
		if !now.Before(e.expiresAt) {
			if _, loaded := c.m.LoadAndDelete(k); loaded {
				c.size.Add(-1)
			}
		}
		return true
	})
}

func (c *workDirResolveCache) store(key, resolved string, now time.Time) {
	if c == nil {
		return
	}
	// R20260527-SEC-4 (#1273): bound the entry count. When at-or-above
	// the cap, sweep expired entries first; if still over, drop the new
	// insert. The next call simply pays EvalSymlinks again — same as a
	// cold boot — so correctness is unaffected. We accept that a key
	// already in the map will Store-overwrite without going through the
	// cap check (that path doesn't grow size), so the effective cap is
	// "distinct live keys" which is exactly what we want to bound.
	if c.size.Load() >= workDirResolveCacheMaxEntries {
		// Fast path: maybe this key is already in the map (Store would
		// be an overwrite, no growth) — let the LoadOrStore branch below
		// distinguish.
		if _, exists := c.m.Load(key); !exists {
			c.sweep(now)
			if c.size.Load() >= workDirResolveCacheMaxEntries {
				return
			}
		}
	}
	entry := workDirResolveCacheEntry{
		resolved:  resolved,
		expiresAt: now.Add(workDirResolveCacheTTL),
	}
	if _, loaded := c.m.Swap(key, entry); !loaded {
		c.size.Add(1)
	}
}

// workDirResolveCacheKey concatenates the three inputs with separators
// that are not valid in absolute paths (`\x00`) so distinct triples
// cannot collide on a single key. R247-PERF-24.
func workDirResolveCacheKey(workDir, allowedRoot, allowedRootResolved string) string {
	return workDir + "\x00" + allowedRoot + "\x00" + allowedRootResolved
}

// workDirResolveUnderRoot is the variant of workDirUnderRoot that also
// returns the symlink-resolved workDir on success. R246-GO-12: callers
// that subsequently hand workDir to a CLI (cli wrapper / claude spawn)
// should use the resolved path so the open-time view matches the
// validation-time view. Without this the workDir we just validated may
// resolve differently when the CLI re-runs EvalSymlinks (TOCTOU window),
// re-introducing the symlink-swap escape that EvalSymlinks-on-validate
// was meant to close.
//
// Returned path is filepath.Clean'd (EvalSymlinks already does that).
// On the empty-workDir / empty-root short-circuit returns ("", true)
// so the caller leaves opts.Workspace untouched (router default applies).
// workDirResolveUnderRootCached is the Scheduler-scoped variant that
// memoises positive results in s.workDirCache. The pure
// workDirResolveUnderRoot below stays the canonical correctness path —
// cold callers (loadJobs / UpdateJob) keep using it because they run
// once per operator action and a stale-cached resolve would mask a
// deliberate retarget. R247-PERF-24.
func (s *Scheduler) workDirResolveUnderRootCached(workDir string) (string, bool) {
	if s == nil {
		return workDirResolveUnderRoot(workDir, "", "")
	}
	now := time.Now()
	// R20260526-PERF-002 (#1225): use precomputed suffix to avoid the
	// three-segment concat allocation each tick. Equivalent to
	// workDirResolveCacheKey(workDir, s.allowedRoot, s.allowedRootResolved).
	key := workDir + s.workDirCacheKeySuffix
	if resolved, ok := s.workDirCache.lookup(key, now); ok {
		return resolved, true
	}
	resolved, ok := workDirResolveUnderRoot(workDir, s.allowedRoot, s.allowedRootResolved)
	if ok {
		s.workDirCache.store(key, resolved, now)
	}
	return resolved, ok
}

func workDirResolveUnderRoot(workDir, allowedRoot, allowedRootResolved string) (string, bool) {
	if workDir == "" || allowedRoot == "" {
		return "", true // empty WorkDir uses router default; empty root = disabled
	}
	if !filepath.IsAbs(workDir) {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		// Missing directory / permission denied — refuse to execute rather
		// than silently re-create the sandbox escape.
		return "", false
	}
	rootResolved, err := filepath.EvalSymlinks(allowedRoot)
	if err != nil {
		// Fall back to the construction-time cached resolution. If neither
		// the per-call EvalSymlinks nor the cache produced a resolved path,
		// hard-reject — comparing the symlink-resolved workDir against the
		// raw allowedRoot string opens a TOCTOU/symlink escape window:
		// allowedRoot="/data/workspace" might resolve to
		// "/mnt/disk0/workspace", in which case a raw-prefix compare against
		// "/data/workspace" would either reject every legitimate child OR
		// (if a subsequent rename swapped the symlink target) admit a
		// path under an attacker-controlled tree. R243-SEC-9 (#795).
		if allowedRootResolved == "" {
			return "", false
		}
		rootResolved = allowedRootResolved
	}
	if resolved == rootResolved {
		return resolved, true
	}
	if strings.HasPrefix(resolved, rootResolved+string(filepath.Separator)) {
		return resolved, true
	}
	return "", false
}

// NewScheduler creates a scheduler. Call Start() to begin.
// applyDefaults fills in zero-valued fields with their package-level
// defaults and clamps oversized values. R232-ARCH-14: extracted from
// NewScheduler so callers (especially tests that build SchedulerConfig
// directly) can see the full default set in one place rather than tracing
// through the constructor body.
//
// Idempotent — calling it on an already-defaulted config is a no-op.
//
// R246-GO-21: pointer receiver mutates in place to avoid copying ~280 bytes
// of SchedulerConfig on every call. Callers that need to preserve the
// original should copy before invoking.
func (cfg *SchedulerConfig) applyDefaults() {
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
}

func NewScheduler(cfg SchedulerConfig) *Scheduler {
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
	if cfg.Router == nil && !cfg.AllowNilRouter {
		slog.Error("cron.NewScheduler: cfg.Router is nil; dashboard sidebar entries will not be created and executeOpt will short-circuit. Set SchedulerConfig.Router on the production wireup, or SchedulerConfig.AllowNilRouter=true on tests that intentionally exercise router-less paths.")
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
	// configured, executeOpt reads a zero AgentOpts (empty Backend / Model
	// / Workspace) and the cron tick spawns with backend defaults
	// silently. Logging at construction makes the misconfiguration visible
	// without changing runtime behaviour.
	if _, ok := cfg.Agents["general"]; !ok {
		slog.Debug("cron: 'general' agent missing from agents map; cron jobs without slash-prefix will fall back to backend defaults",
			"agent_count", len(cfg.Agents))
	}
	s := &Scheduler{
		cron: robfigcron.New(
			robfigcron.WithLocation(loc),
			robfigcron.WithChain(
				robfigcron.Recover(cronLogger),
				robfigcron.SkipIfStillRunning(cronLogger),
			),
		),
		jobs:                make(map[string]*Job),
		chatJobCount:        make(map[chatJobKey]int),
		jobsByChat:          make(map[chatJobKey][]*Job),
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
	}
	// R250-ARCH-14: initialise the per-Scheduler marshal seam so the
	// hot path in marshalJobsLocked finds defaultMarshalJobs instead of
	// nil. Tests swap a failing stub via withFailingMarshal.
	s.marshalJobs.Store(&defaultMarshalJobs)
	// R20260527-GO-1: install the broadcaster via atomic.Pointer so
	// later SetTelemetry calls are race-free vs the cron-dispatch read
	// path in emitRunStarted / emitRunEnded. nil cfg.Telemetry leaves
	// the pointer as zero-value (no broadcast).
	if cfg.Telemetry != nil {
		b := cfg.Telemetry
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
				if err := os.MkdirAll(dir, 0o700); err != nil {
					slog.Warn("cron store parent dir mkdir failed (eager)", "err", err, "dir", dir)
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
		// R246-CR-246: also reset startedAtNanos. The original comment
		// above the Store claimed "下次重试会覆盖" but that only holds if
		// the next Start() actually succeeds quickly. If retry is delayed,
		// the stale startedAtNanos timestamp would feed HasMissedSchedule's
		// startup-suppression window (currently NOT yet running because
		// started=false but exposed via StartedAt() to callers like
		// dashboard / metrics) and report a false "running since N min ago"
		// state. Clearing it keeps StartedAt() == zero until a Start() that
		// actually hands off to cron runner.
		s.started.Store(false)
		s.startedAtNanos.Store(0)
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
			key := chatJobKey{Platform: j.Platform, ChatID: j.ChatID}
			s.chatJobCount[key]++
			s.jobsByChat[key] = append(s.jobsByChat[key], j)
			stubs = append(stubs, stubRow{j.ID, j.WorkDir, j.Prompt, j.LastSessionID})
			continue
		}
		if err := s.registerJob(j); err != nil {
			slog.Warn("skip invalid cron job", "job_id", j.ID, "schedule", j.Schedule, "err", err)
			continue
		}
		s.jobs[j.ID] = j
		key := chatJobKey{Platform: j.Platform, ChatID: j.ChatID}
		s.chatJobCount[key]++
		s.jobsByChat[key] = append(s.jobsByChat[key], j)
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
			// R234-GO-3 / #1019: 传 stopCtx 进 trimAll，Stop 可在 job 入口
			// 之间中断长时间的 GC 扫描，避免 Stop 等到 gcWaitBudget。
			s.runStore.trimAllCtx(s.stopCtx, time.Now())
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
//
// R241-ARCH-6 (#510): the historical "silent no-op when router is nil"
// hid wireup bugs because the misconfiguration only surfaced as a
// missing dashboard sidebar entry, not as a startup or first-tick
// failure. The construction-time slog.Error in NewScheduler now flags
// the missing wiring loud at boot; this callsite additionally logs the
// first time it would have refreshed a stub but couldn't (sync.Once
// gate so a router-less test fixture / AllowNilRouter deployment does
// not spam the log across N ticks).
func (s *Scheduler) registerStubByValue(id, workDir, prompt, lastSessionID string) {
	if s.router == nil {
		s.routerNilOnce.Do(func() {
			slog.Error("cron: registerStubByValue called without a router; dashboard sidebar will be empty for this scheduler — wireup bug or missing SchedulerConfig.Router?",
				"job_id", id)
		})
		return
	}
	var chain []string
	if lastSessionID != "" {
		chain = []string{lastSessionID}
	}
	s.router.RegisterCronStubWithChain(sessionkey.CronKey(id), workDir, prompt, chain)
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
	if !sessionkey.IsCronKey(key) {
		return false
	}
	id := key[len(sessionkey.CronKeyPrefix):]
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

// StopPolicy is the documented Stop-overflow strategy this Scheduler
// honours: when the per-call wait budget elapses with goroutines still
// in flight, Stop logs a warning and proceeds to the final persist,
// leaving any orphaned goroutines for the OS to reap on process exit.
//
// Why this is a string constant rather than a typed enum: cron and
// sysession independently document their Stop-overflow strategies
// (sysession uses StopPolicyForceExit — see
// internal/sysession/manager.go) and the divergence is a deliberate
// security decision (Sec-LOW-2: sysession daemons run user-prompt-
// derived strings through a CLI subprocess, so a stuck goroutine
// touching a torn-down router could echo conversation excerpts back
// to a different session's reply path; cron deliveries do not have
// that surface). Mechanically unifying the two via a shared enum
// would invite the wrong "harmonise the strategies" intuition. Each
// package exposes its own string constant operators can grep.
//
// Closes #1060 (R244-ARCH-7) — promotes the implicit decision (live
// only in comments inside Stop's godoc + R49-REL-CRON-STOP-BUDGET
// linkage) to a typed constant operators can reference in alerts /
// runbooks. NOT used in cron's control flow today; intentionally
// doc-only so future "let's check policy at runtime" callers must
// add the comparison and its tests deliberately.
const StopPolicyBudgetThenLeak = "budget_then_leak"

// defaultStopBudget is the production overall deadline Scheduler.Stop()
// will spend waiting on cron.Stop + triggerWG before proceeding to save.
// Shared between both waits (not doubled per wait) so a production
// deployment with execTimeout=3600s cannot pin restart for ≈2 h — the
// prior two-budget design had a worst case of 2×(execTimeout+5s).
// Aligned with session.ShutdownTimeout (30s) so both subsystems agree on
// the upper bound systemd sees. R49-REL-CRON-STOP-BUDGET.
const defaultStopBudget = 30 * time.Second

// gcWaitBudget bounds the cold-start GC goroutine wait in Stop(). Smaller
// than defaultStopBudget because trimAll's IO is short-lived
// (ReadDir + N Removes); a wedge here means a stuck filesystem and we'd
// rather skip the wait than pin systemd TimeoutStopSec.
//
// R247-CR-18: kept as a const because no production / test path needs to
// shorten it. If you find yourself wanting to override per-test, use a
// `*time.Timer` injected via a Scheduler field instead of reintroducing
// a package-level var — package vars under t.Parallel races silently.
const gcWaitBudget = 5 * time.Second

// stopBudget is the active stop budget used by Scheduler.Stop(). Tests
// MUST mutate it only through the WithStopBudget seam in
// scheduler_testutil_test.go so the var swap is paired with a
// t.Cleanup restore — direct writes from t.Parallel tests would race
// a concurrent Stop on another Scheduler instance with real
// wall-clock timeouts.
var stopBudget = defaultStopBudget

// (R248-DEADCODE-24 / #1216) WithStopBudget moved to
// scheduler_testutil_test.go: it is test-only and previously living in
// production scheduler.go pinned dead surface area in the production
// binary. Same-package _test.go retains access to the unexported
// stopBudget without changing test call sites.

// Stop halts the scheduler and saves state. It waits for both scheduled jobs
// (drained by s.cron.Stop) and any TriggerNow-spawned goroutines before
// returning, so callers can safely tear down the router afterwards.
//
// Shutdown wall-clock is bounded by gcWaitBudget + stopBudget (5s + 30s
// = 35s default; both are independent timers — gcWG.Wait runs first to
// completion or timeout, *then* the stopBudget deadline starts for
// cron.Stop + triggerWG). R247-GO-14: prior godoc only mentioned the 30s
// stopBudget, which under-stated worst-case Stop() wall-clock by the
// 5s gc wait when the cold-start GC goroutine is wedged on a stuck
// filesystem.
//
// A stuck cron job (execute() hanging past ctx cancel, e.g. a broken
// shim ignoring context) or a stuck triggerWG (deliverNotice → platform
// Reply webhook that refuses to honour its own timeout) cannot hold us
// past stopBudget. A stuck trimAll cannot hold us past gcWaitBudget. The
// final saveJobs runs regardless so a stuck drain does not cost the
// state file. Tests overriding budgets via WithStopBudget /
// WithGCWaitBudget on the *Scheduler instance see the same composition.
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
//
// The same intentional-orphan contract applies to the gcWG.Wait wrapper
// goroutine spawned just below (the cold-start GC waiter). When
// gcWaitBudget elapses with trimAll still wedged on a stuck filesystem
// (ReadDir / Remove not returning), the wrapper around `gcWG.Wait()`
// stays parked and is reclaimed only when the OS tears the process
// down. Rationale is identical: Scheduler is single-shot, the process
// is moments from exit, and gcWG offers no cancel signal. If a future
// reuse path is added (Start after Stop on the same instance), this
// wrapper MUST also be migrated to a ctx-aware pattern (e.g. trimAll
// observing stopCtx) so successive lifecycles do not accumulate stuck
// filesystem-IO goroutines until OOM. R247-GO-7.
//
// THIRD intentional-orphan site (R250-GO-9 / #1072): runDeadlineWatchdog
// in scheduler_run.go spawns a goroutine that parks on the run's sendCtx
// outside the triggerWG accounting. On the success path the caller's
// sendCancel() unblocks <-ctx.Done() and the goroutine returns; on the
// stuck-Send path (CLI ignoring ctx, shim hanging) the watchdog stays
// parked until Send eventually returns or the OS reclaims it. The
// goroutine holds only the abortCh send (buffer=1, so the send itself
// does not block) — no triggerWG.Add is held, so Stop()'s budget never
// waits on it. Acceptable on the same single-shot-Scheduler grounds as
// triggerWG / gcWG, with one extra leak source operators should know
// exists when reading "deadline fired but interrupt did not land"
// post-Stop log lines. If a future reuse path is added the watchdog
// MUST also be migrated under triggerWG so its lifetime is bounded by
// the same stopBudget the Send-spawning code is.
func (s *Scheduler) Stop() {
	// R20260526-GO-007: idempotent CAS guard. Without this, repeat calls
	// re-enter the timer-allocating + persist branches below — wasting
	// time.NewTimer slots, double-running persistJobsLocked, and racing the
	// final marshaled write against itself. Mirror Start()'s `started`
	// CAS so the lifecycle is symmetrically idempotent. stopCancel is
	// already idempotent (context cancel is a no-op after the first call),
	// so callers that bypass this guard via earlier wiring are unaffected.
	if !s.stopped.CompareAndSwap(false, true) {
		return
	}
	s.stopCancel()

	// R247-CR-4 (#584): the four shutdown stages each own an explicit
	// budget; Stop() orchestrates them in order. Each helper logs a Warn +
	// bumps its CronStopBudgetExceeded* counter on its own deadline; Stop
	// itself contains no budget arithmetic.
	s.waitGCDrain()
	deadlineHit, stopStart := s.drainCronStop()
	if !deadlineHit {
		s.drainTriggerWG(stopStart)
	}
	s.persistOnShutdown()
}

// waitGCDrain blocks until the cold-start GC goroutine spawned in Start()
// completes or gcWaitBudget elapses. Filesystem mutations on the runs/
// tree from trimAll race the upcoming persist + Append-from-triggerWG
// paths if we don't drain first; the budget keeps a wedged trimAll from
// pinning systemd TimeoutStopSec. R236-GO-01 (origin) / R247-CR-4 (extract).
func (s *Scheduler) waitGCDrain() {
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
		// R250-GO-20 (#1083): pair the per-phase Warn with a counter so
		// dashboards can alert on shutdown-budget breaches without
		// grepping journalctl. Useful for catching systemd TimeoutStopSec
		// proximity in production.
		metrics.CronStopBudgetExceededGCTotal.Add(1)
		slog.Warn("cron: gc goroutine wait timeout", "budget", gcWaitBudget)
	}
}

// drainCronStop signals the robfig/cron runner to stop accepting new ticks
// and waits up to stopBudget for in-flight ticks to drain. Returns
// (deadlineHit, stopStart) — caller skips drainTriggerWG when deadlineHit
// is true (the budget is shared across both phases). stopStart anchors the
// remaining-budget arithmetic in drainTriggerWG so both phases account
// against the same wall clock. R246-GO-13 / R247-CR-4.
func (s *Scheduler) drainCronStop() (deadlineHit bool, stopStart time.Time) {
	cronDoneCtx := s.cron.Stop()

	// Single overall deadline shared across both waits. If cron.Stop drains
	// fast we have the full budget for triggerWG; if it eats the budget we
	// skip triggerWG.Wait entirely (the leaked goroutines die when the
	// process exits moments later). Either way saveJobs runs — losing it
	// would undo mutations that had already returned 2xx to dashboard/IM.
	//
	// R246-GO-13: track stopStart and re-derive the remaining budget for
	// the second select via time.After, instead of reusing deadline.C from
	// a NewTimer across two select statements. Reusing a fired timer's
	// channel is a known footgun (the receive cannot be guaranteed to
	// observe the prior firing exactly once across both selects, and Go
	// makes no documented guarantee about timer-channel buffering across
	// independent receivers); a fresh time.After on the remaining budget
	// is the explicit, documented pattern. The first select still uses
	// the NewTimer so we can defer-Stop it on the early-drain path.
	stopStart = time.Now()
	deadline := time.NewTimer(stopBudget)
	defer deadline.Stop()

	select {
	case <-cronDoneCtx.Done():
	case <-deadline.C:
		deadlineHit = true
		// R250-GO-20 (#1083): see GC counter rationale above.
		metrics.CronStopBudgetExceededDrainTotal.Add(1)
		slog.Warn("cron scheduler: stop deadline exceeded before cron.Stop drained, proceeding",
			"budget", stopBudget)
	}
	return deadlineHit, stopStart
}

// drainTriggerWG waits for TriggerNow + deliverNotice goroutines to drain,
// budgeted by the *remaining* share of stopBudget after drainCronStop. Caller
// must skip this phase entirely when drainCronStop's deadlineHit is true so
// the budget is honoured as a single overall ceiling.
//
// R222-GO-10: when the deadline pre-empts triggerDone, the wrapper goroutine
// started by `go func() { s.triggerWG.Wait(); close(...) }` stays parked on
// triggerWG.Wait — exactly the intentional-orphan path documented in the
// Stop CONTRACT block. Reclaim happens when the OS tears the process down.
// We deliberately do NOT add a sync.Once / chan-cancel reclaim path here:
// triggerWG.Wait does not accept a cancel signal, and Scheduler is
// single-shot (Stop is terminal). A goroutine-leak detector running in
// tests that shorten stopBudget to milliseconds will surface this orphan;
// tests that care should plumb a non-stuck deliverNotice fake instead.
//
// Bound triggerWG.Wait with the *remaining* share of the same budget:
// manual TriggerNow respects stopCtx via execute(), and R243-SEC-14
// (#799) wired notifyTarget's replyCtx to s.stopCtx so a hung webhook
// short-circuits on the cancel edge instead of waiting for its own
// per-target timer. The deadline here remains the backstop for any
// notify path that still parents on Background (e.g. a future helper
// or a test fake that bypasses notifyTarget): without it a stuck
// platform HTTP call could otherwise pin Stop() past systemd
// TimeoutStopSec.
//
// R247-CR-4: extracted from Stop().
func (s *Scheduler) drainTriggerWG(stopStart time.Time) {
	triggerDone := make(chan struct{})
	go func() {
		s.triggerWG.Wait()
		close(triggerDone)
	}()
	// R246-GO-13: derive remaining budget from stopStart instead of
	// re-reading deadline.C. If cron.Stop drained at the very edge of
	// the budget, remaining can be near-zero; clamp to a tiny floor so
	// we still observe an instantaneous triggerDone (already-closed
	// channel) without wedging on a 0-duration timer. The clamp is
	// not a guaranteed minimum wait — both the channel and the timer
	// are checked in the same select.
	//
	// R249-GO-4: use NewTimer + defer Stop instead of time.After.
	// time.After returns a fresh timer whose underlying resources
	// are released only when it fires; on the triggerDone-fast path
	// the timer would leak its slot until expiry (~30s default).
	// More urgently, with remaining clamped to 1ms the timer almost
	// certainly fires before the select runs, and a fired channel
	// from time.After is unreachable for explicit Stop. Mirror the
	// first select's NewTimer + defer Stop pattern (line ~820) so
	// both halves of Stop release timer state deterministically.
	remaining := stopBudget - time.Since(stopStart)
	if remaining < time.Millisecond {
		remaining = time.Millisecond
	}
	triggerTimer := time.NewTimer(remaining)
	defer triggerTimer.Stop()
	select {
	case <-triggerDone:
	case <-triggerTimer.C:
		// R250-GO-20 (#1083): see GC counter rationale above.
		metrics.CronStopBudgetExceededTriggerTotal.Add(1)
		slog.Warn("cron scheduler: stop deadline exceeded during triggerWG wait, proceeding",
			"budget", stopBudget, "remaining_ms", remaining.Milliseconds())
	}
}

// persistOnShutdown runs the final cron_jobs.json write through
// persistJobsLocked + saveSeq gate. Routing through the gate (not a bare
// WriteFileAtomic) keeps a queued-but-not-landed mutator save from later
// overwriting Stop's snapshot with stale data — R232-ARCH-10. R246-GO-5
// (#690) tags failures with persist=FAILED_DURING_SHUTDOWN so log
// aggregation routes them to the unrecoverable-data-loss alert channel
// (the per-mutation "save cron store" failure is recoverable on the next
// mutation; this one is not, the process is moments from exit).
//
// R247-CR-4: extracted from Stop().
func (s *Scheduler) persistOnShutdown() {
	s.mu.Lock()
	save, err := s.persistJobsLocked()
	s.mu.Unlock()
	if err != nil {
		slog.Error("marshal cron store on shutdown",
			"err", err,
			"persist", "FAILED_DURING_SHUTDOWN")
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
//
// R247-GO-11: also defensive against a nil receiver. Sibling getters
// (StartedAt / KnownSessionIDs) already short-circuit on a nil
// *Scheduler so test fixtures can construct a partial Scheduler and
// invoke deletion paths without dereferencing s. Without this guard a
// test calling DeleteJobByID on a zero-value scheduler — or production
// code that has not yet wired router — would NPE on s.router access
// rather than returning quietly.
func (s *Scheduler) resetRouterStub(jobID string) {
	if s == nil {
		return
	}
	if s.router == nil {
		return
	}
	s.router.Reset(sessionkey.CronKey(jobID))
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

// cronPanicMarker is the substring scanned in robfig/cron-emitted log
// lines to escalate to slog.Error rather than slog.Warn. Pulled out as a
// named const (R247-CR-23) so call-site readers see WHAT we look for and
// WHY in one place — the previous inline `strings.Contains(msg, "panic")`
// read as a negative assertion ("if this is a panic line") that obscured
// the upstream-stability rationale baked into the comment.
//
// robfig/cron's Recover wrapper invokes logger.Error(err, "panic",
// "stack", ...) (chain.go ~line 50); the printfLogger Error formatter
// renders the msg argument verbatim, so the literal substring "panic"
// is guaranteed to appear in every recover-emitted line. No other Error
// path through the library carries this token.
//
// R249-CR-24: dropped the historical cronRecoveredMarker = "recovered"
// fallback. It existed as a forward-compat hedge for a hypothetical
// upstream rename of the Recover message but never matched real output:
// robfig/cron 3.0.x emits "panic" only, and a future rename would arrive
// in a Go module bump where we'd update the marker alongside any other
// breakage. Single Contains scan is enough — keeping a no-op fallback
// added a strings.Contains call per emitted line for no observed signal.
const cronPanicMarker = "panic"

func (slogPrintfLogger) Printf(format string, args ...any) {
	// R250-CR-15 (#1148): skip fmt.Sprintf when there are no args. Saves
	// an alloc per emitted line and avoids passing untrusted format
	// verbs through the formatter (robfig/cron's PrintfLogger.Error and
	// Info both call Printf with the message as the first arg, which
	// can contain user-controlled content like cron spec strings).
	var msg string
	if len(args) == 0 {
		msg = format
	} else {
		msg = fmt.Sprintf(format, args...)
	}
	msg = strings.TrimRight(msg, "\n")
	if strings.Contains(msg, cronPanicMarker) {
		slog.Error("cron logger", "msg", msg)
		return
	}
	slog.Warn("cron logger", "msg", msg)
}
