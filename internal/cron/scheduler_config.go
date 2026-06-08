package cron

// scheduler_config.go holds the config/construction-domain declarations
// split out of scheduler.go (move-only, #1282): the cron sentinels, the
// SessionRouter consumer interface, SchedulerConfig + its defaulting helpers
// (applyDefaults / resolveAllowedRoot), the chatJobKey index key, and the
// cronConfigMaps snapshot. No behaviour changed — these symbols relocated
// verbatim and remain same-package members of the cron Scheduler.

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/runtelemetry"
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

// ErrPromptAlreadySet is returned by SetJobPrompt when the target job already
// has a non-empty prompt. SetJobPrompt only auto-fills the FIRST prompt on a
// dashboard-created (paused, empty-prompt) job; it deliberately does NOT
// overwrite an existing prompt — that is UpdateJob's job. Previously this was
// a silent `return nil`, so a caller intending to edit an existing prompt got
// a 200 with no change (R250531-CR-8 / #1503). The sentinel makes the no-op
// observable: IM auto-save callers treat it as benign (the prompt was already
// captured on a prior turn), while any caller that actually meant to mutate
// should route through UpdateJob and can map this to HTTP 409 Conflict.
var ErrPromptAlreadySet = errors.New("cron: job already has a prompt; use UpdateJob to change it")

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

// ErrSchedulerStopped is returned by Start() when the scheduler has already
// been Stop()'d. Stop() documents that the instance is single-shot — its
// budget-exceed paths intentionally leak wrapper goroutines because no reuse
// is expected; reviving a stopped instance would accumulate those orphans
// across lifecycles. R249-ARCH-19 (#984): sentinel so callers can errors.Is
// and distinguish "already stopped" from a transient loadJobs failure.
var ErrSchedulerStopped = errors.New("cron: scheduler already stopped")

// SessionRouter is the subset of session.Router that the cron Scheduler
// actually consumes. Declaring it here (consumer-side interface, Go idiom)
// inverts the historical cron → session dependency: a future Router refactor
// only has to preserve these three method shapes to stay scheduler-
// compatible, and tests can inject a fake without pulling the whole router
// graph. Any new s.router.X() call requires extending this interface, which
// makes accidental surface-area growth a compile error instead of a silent
// regression.
//
// R238-ARCH-7 (#752) is closed by the current shape: GetOrCreate already
// returns cron-local types (Session interface + SessionStatus) — see
// agent_opts.go for the Send/SessionID/InterruptViaControl method set —
// so cron does NOT transitively depend on *session.ManagedSession. The
// production wireup wraps the concrete session in cronSessionAdapter
// (internal/wireup/cron_router_adapter.go); the InterruptOutcome ordinal pin
// + the SessionRouter compile-time guard live there too. The cron
// package no longer imports internal/session in production code (last
// reverse import eliminated by R20260527122801-ARCH-1).
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
	// The production wireup (internal/wireup/cron_router_adapter.go) wraps
	// *session.ManagedSession in a cron.Session adapter.
	GetOrCreate(ctx context.Context, key string, opts AgentOpts) (Session, SessionStatus, error)
}

// SchedulerConfig holds configuration for the cron scheduler.
//
// Config convention (RFC cron-config-and-structs Phase 1, ratified #776):
// the scheduler stays struct-config rather than functional-options. Fields
// split into two classes; the split is part of the API contract and is
// pinned by scheduler_config_construction_test.go.
//
//   - REQUIRED (no zero-value fallback): Router. Omitting it (without
//     AllowNilRouter) makes NewScheduler emit a boot-time slog.Error and
//     leaves executeOpt/registerStub short-circuiting — there is no default
//     router. StorePath is "soft-required": empty means in-memory only (no
//     persistence), which is valid for tests but is a misconfig in prod.
//
//   - OPTIONAL (documented zero-value fallback, applied in applyDefaults /
//     resolveAllowedRoot):
//     MaxJobs        — <=0 → defaultMaxJobs (50); clamped to maxJobsHardCap (500).
//     MaxJobsPerChat — <=0 → DefaultMaxJobsPerChat (10); cannot be disabled (R208-BL2).
//     ExecTimeout    — <=0 → defaultExecTimeout (5m).
//     Location       — nil → time.Local.
//     SlowThreshold  — <=0 → defaultCronSlowThreshold (read lazily at the callsite).
//     AllowedRoot    — "" → no root constraint; NUL-bearing → cleared (loud).
//     RunsKeepCount / RunsKeepWindow — <=0 → DefaultRunsKeepCount / DefaultRunsKeepWindow.
//     JitterMax      — 0 → jitter disabled.
//     Telemetry / NotifyDefault / Agents / NotifySender / AgentCommands /
//     ParentCtx / AllowNilRouter — idiomatic-optional (nil/zero = feature off).
//
// applyDefaults() is pure/idempotent and is the single source of truth for
// the numeric/Location fallbacks above; AllowedRoot resolution lives in
// resolveAllowedRoot() because it does a syscall.
type SchedulerConfig struct {
	// Router is the session router the scheduler talks to. Accepts the
	// SessionRouter interface so tests can pass a minimal fake; production
	// passes a *session.Router which satisfies it transparently.
	Router SessionRouter
	// NotifySender resolves a platform name to its PlatformReplier for cron
	// completion notices. #725: replaces the former
	// Platforms map[string]platform.Platform so internal/cron no longer
	// imports internal/platform — the wireup layer builds a
	// platformNotifySender adapter over the live platform map. nil = no
	// notify delivery (the Lookup miss path keeps notifyTarget's existing
	// "platform not found" WARN).
	NotifySender  NotifySender
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

// chatKeyFor builds the chatJobKey for a (platform, chatID) pair. R249-CR-4 /
// R260528-ARCH-7 (#948 / #1368): the bare `chatJobKey{Platform: p, ChatID: c}`
// literal was open-coded at the read sites (JobCountForChat / ListJobs /
// ListJobsWithNextRun / findByPrefixLocked); a single constructor keeps the
// (platform, chatID) → key mapping in one place alongside the jobsByChat /
// chatJobCount trio it indexes, so a future change to the key shape (e.g.
// folding in a backend dimension) is a one-site edit.
func chatKeyFor(plat, chatID string) chatJobKey {
	return chatJobKey{Platform: plat, ChatID: chatID}
}

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

// resolveAllowedRoot sanitises cfg.AllowedRoot (clearing NUL-bearing values
// loud) and returns the EvalSymlinks-resolved form for the sanitised root.
// Empty result on either side means "no root constraint" — the empty-string
// branch in workDirResolveUnderRoot then short-circuits the per-call check.
//
// Split out from NewScheduler under R241-ARCH-10 (#517) so the constructor
// body reads as a near-flat field mirror. Kept off of applyDefaults because
// EvalSymlinks is a syscall and applyDefaults is documented as pure /
// idempotent / safe to call from tests.
//
// R20260527-PERF-14 (#1297) (preserved): an operator-set NUL byte in
// AllowedRoot would tokenise workDirCacheKeySuffix incorrectly and cause
// workDirResolveUnderRootCached to alias unrelated workDir inputs onto the
// same cache slot. NUL also has no legitimate place in a filesystem path on
// POSIX or Windows. The AllowedRoot is cleared so workDir checks fall
// through to "no root constraint", with a loud slog.Error to surface the
// misconfig.
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
		// R112714-LOGIC-4: EvalSymlinks failed (e.g. path does not exist yet,
		// or is a dangling symlink). Silently returning "" disabled the
		// allowed-root sandbox for the entire scheduler lifetime with no
		// operator-visible signal. Log a warning and fall back to the raw
		// string: bare string comparison is weaker than symlink-resolved
		// comparison (symlinks under AllowedRoot could escape the check) but
		// it is strictly better than disabling the constraint entirely.
		// Operators should fix the root path; the warning makes the misconfig
		// visible in logs at startup.
		slog.Warn("cron.NewScheduler: filepath.EvalSymlinks failed for AllowedRoot; "+
			"using raw path (symlinks under this root may bypass the constraint)",
			"allowed_root", cfg.AllowedRoot, "err", err)
		return cfg.AllowedRoot
	}
}

// cronConfigMaps bundles the three write-once-then-immutable config maps so
// a single atomic.Pointer can publish them as one consistent snapshot (see
// the configMapsPtr field godoc, R249-ARCH-27 / #991): readers Load() the
// pointer and treat the maps as read-only; a future hot-reload writer
// rebuilds a fresh *cronConfigMaps (copy-on-write) and Store()s it, so no
// reader ever observes a torn cross-map state and no per-field lock is
// needed.
type cronConfigMaps struct {
	// notifySender is published inside this same atomic snapshot as
	// agents/agentCommands (R249-ARCH-27 / #991): notifyTarget reads it via
	// s.configMaps() without s.mu, so it MUST share the single
	// atomic.Pointer[cronConfigMaps] rather than living in its own field, or
	// a reader could observe a torn cross-field snapshot. #725: it is an
	// interface value (write-once at NewScheduler), so unlike the maps it is
	// not cloned — there is no backing array to alias.
	notifySender  NotifySender
	agents        map[string]AgentOpts
	agentCommands map[string]string
}

// emptyConfigMaps is the non-nil zero-value snapshot returned by
// configMaps() for a hand-constructed *Scheduler (test fixtures) that never
// Store()d a pointer. All three maps are nil; indexing a nil map is a safe
// zero-value read, exactly mirroring the prior bare-nil-field semantics so
// callers like notifyTarget keep working without a NewScheduler round-trip.
var emptyConfigMaps = &cronConfigMaps{}

// configMaps returns the current immutable config-map snapshot. Never nil:
// NewScheduler always Store()s a populated *cronConfigMaps; a *Scheduler
// built directly via &Scheduler{} (tests) gets emptyConfigMaps so the
// lock-free readers (notifyTarget / executeOpt) never nil-deref. The maps
// inside may be nil when the caller omitted them — maps.Clone(nil) == nil —
// but indexing a nil map is a safe zero-value read.
func (s *Scheduler) configMaps() *cronConfigMaps {
	if cm := s.configMapsPtr.Load(); cm != nil {
		return cm
	}
	return emptyConfigMaps
}
