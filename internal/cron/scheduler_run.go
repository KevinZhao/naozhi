// scheduler_run.go: cron run execution path.
//
// Contains the heart of cron: executeOpt (the 344-line state machine that
// CASes the inflight gate, jitters, snapshots Job fields, runs the
// fresh-context preflight, spawns/sends through the session router,
// guards send with a deadline watchdog, and routes terminal state to
// finishRun) plus its helpers — preflight, snapshot, watchdog, error
// classification, jitter, and the per-job runInflight allocator.
//
// Split out of scheduler.go to keep the run-time hot path (which adds
// Spans, error classes, and observability hooks more often than any other
// area of cron) in one place and isolate it from CRUD / persist / notify
// concerns. No behaviour change; methods stay on *Scheduler so private
// fields remain accessible without exporting.

package cron

import (
	"context"
	"errors"
	"expvar"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/apierr"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/sessionkey"

	robfigcron "github.com/robfig/cron/v3"
)

// defaultCronSlowThreshold, spawnElapsedWarnRatio and minSendBudget are
// defined in tuning.go (R249-CR-16, #959), which collects all cron tuning
// knobs into one place with an operator-facing raise/lower table.

// executeIfNotDeletedOrPaused is the TriggerNow dispatch entry. It looks
// up the freshest *Job under s.mu.RLock, then — only if still present and
// not paused — releases the lock and calls executeOpt(cur, true). Deleted
// or paused jobs surface as a Debug-log skip with no run record.
//
// LOCK: caller MUST NOT hold s.mu (this acquires s.mu.RLock); robfig/cron
// MUST NOT hold its internal cron lock when invoking this (the snapshot →
// release → executeOpt split exists precisely so executeOpt's long-running
// send/notify pipeline never runs under s.mu).
//
// R238-GO-9 (#801): TriggerNow's goroutine bypasses the robfig/cron chain's
// Recover wrapper that protects the scheduled-tick path, so a panic in
// executeOpt would propagate up to the TriggerNow goroutine and kill it
// (and any inflight defer that hadn't fired yet — the deferred Done in
// scheduler_jobs.go's TriggerNow closure DOES still fire because it's
// registered before this call, but the panic surfaces as a runtime crash
// in the slog of the goroutine that ran it). Recover here so a panicking
// job fails loud once (Error log + stack) and the surrounding goroutine
// still completes. The scheduled path keeps robfig's Recover and does NOT
// pass through this helper — registerJob's AddFunc closure routes through
// executeJobIDIfLive directly so we don't double-recover.
func (s *Scheduler) executeIfNotDeletedOrPaused(jobID string) {
	defer func() {
		if r := recover(); r != nil {
			recordTriggerNowPanic(jobID, r)
		}
	}()
	s.executeJobIDIfLive(jobID, true /* viaTriggerNow */, "TriggerNow")
}

// recordTriggerNowPanic logs a TriggerNow-path panic. Split out of
// executeIfNotDeletedOrPaused's defer so the recover site stays a one-
// liner and the formatted-log path is exercisable in tests without
// deferring inside the test body. R238-GO-9 (#801).
func recordTriggerNowPanic(jobID string, r any) {
	slog.Error("TriggerNow: panic recovered, run abandoned",
		"job_id", jobID,
		"panic", r,
		"stack", string(debug.Stack()))
}

// executeJobIDIfLive is the shared lookup-and-dispatch primitive used by
// both TriggerNow (executeIfNotDeletedOrPaused) and the registerJob
// AddFunc closure (R247-CR-10). Both paths previously open-coded the
// RLock → exists/paused check → executeOpt fan-out with only the
// viaTriggerNow flag and Debug log subject differing; the duplicated
// closure made it easy to drift one path's pre-flight gate without the
// other. logSubject is the caller-supplied prefix used in skip Debug
// logs so operators distinguish "TriggerNow:" vs "cron:" in the
// shutdown / pause race traces.
func (s *Scheduler) executeJobIDIfLive(jobID string, viaTriggerNow bool, logSubject string) {
	s.mu.RLock()
	cur, ok := s.jobs[jobID]
	paused := ok && cur.Paused
	s.mu.RUnlock()
	// R243-ARCH-13 (#841): bind the {subject, job_id} label pair once via
	// slog.With instead of re-listing the same two keys at every skip-log
	// site. Keeps the two skip-branch Debug lines from drifting their label
	// set apart. Constructed lazily (only on skip path) to avoid ~500
	// wasted allocs/sec on the hot live-job path (R093146-PERF-1).
	if !ok || paused {
		lg := slog.With("subject", logSubject, "job_id", jobID)
		if !ok {
			lg.Debug("job deleted before execute, skipping")
		} else {
			lg.Debug("job paused concurrently, skipping")
		}
		return
	}
	s.executeOpt(cur, viaTriggerNow)
}

// cleanupRunningJobIfIdle drops the s.runningJobs entry for jobID iff
// the runInflight CAS gate is currently false (no in-flight execute()
// holds it). R242-ARCH-15 (#758): the prior policy was "never clean,
// bounded by maxJobsHardCap=500" — but a long-lived deployment that
// adds and deletes thousands of cron jobs over weeks accumulates an
// unbounded sync.Map of dead *runInflight structs, never freed.
//
// The original ID-reuse split-CAS concern (a fresh AddJob colliding on
// the same 16-hex-char ID while the old execute() still holds the
// pointer to the OLD guard) is mitigated three ways at this entry
// point:
//
//  1. We only LoadAndDelete when running.Load() == false. If the gate
//     is held, leave the entry alone — the executeOpt goroutine still
//     holds the pointer and is about to releaseRun() on it. The caller
//     can re-attempt cleanup later (or accept the per-job-id leak;
//     bounded by jobs that get deleted while a run is in flight, an
//     even narrower window than the original maxJobsHardCap bound).
//  2. ID generation is crypto/rand 8 bytes (16 hex chars, 2^64 space);
//     for the maxJobsHardCap=500 working set the birthday-paradox
//     collision probability is ~2^-32 over the entire process lifetime
//     — far below any other production race we accept.
//  3. AddJob already retries on s.jobs collision (10 attempts, slog.Warn
//     each) so a re-used ID would not silently slip in undetected; the
//     window where new AddJob lands BEFORE the old run finishes is
//     vanishingly thin.
//
// Returns true if the entry was deleted. Safe to call after s.mu is
// released — sync.Map.LoadAndDelete needs no scheduler lock. Callers
// invoke this from postCleanup branches that already run lock-free.
func (s *Scheduler) cleanupRunningJobIfIdle(jobID string) bool {
	v, ok := s.runningJobs.Load(jobID)
	if !ok {
		return false
	}
	inf, ok := v.(*runInflight)
	if !ok || inf == nil {
		// Defensive: an unexpected map value type implies the package
		// invariant was violated upstream.
		//
		// R040034-GO-7 (#1392): bump severity to slog.Error so the
		// invariant violation surfaces in journalctl. The previous
		// silent sweep meant a future regression that stored the wrong
		// type into runningJobs (a refactor returning a value-typed
		// snapshot, a stale closure, etc.) would be cleaned up without
		// any operator-visible signal until downstream code paths
		// observed the missing in-flight metadata.
		//
		// R260528-BUG-11: use CompareAndDelete on the observed v (not
		// LoadAndDelete on the jobID) so a concurrent jobInflight that
		// already replaced this stale entry with a fresh *runInflight
		// is not collateral damage. Mirrors the normal-path
		// CompareAndDelete below to keep both branches TOCTOU-safe
		// under the same single-flight contract.
		slog.Error("cron: runningJobs holds unexpected value type; sweeping",
			"job_id", jobID, "type", fmt.Sprintf("%T", v))
		s.runningJobs.CompareAndDelete(jobID, v)
		return true
	}
	if inf.running.Load() {
		// In-flight execute() goroutine still holds the pointer and is
		// about to releaseRun(); skip — leaking THIS one entry until the
		// next DeleteJob sweep is cheaper than risking a CAS-gate split
		// against a (vanishingly rare) ID-reuse collision.
		return false
	}
	// R20260527-GO-2 (#1270): use CompareAndDelete on the *runInflight
	// pointer rather than LoadAndDelete on the key. The Load+LoadAndDelete
	// pair is non-atomic — between the running.Load() check and the
	// LoadAndDelete, a concurrent executeOpt for an ID-reused jobID can
	// CompareAndSwap the gate to true and we'd then drop the now-active
	// entry. The next jobInflight call would LoadOrStore a fresh
	// *runInflight, leaving two goroutines holding distinct gate pointers
	// for the same jobID → double execution. CompareAndDelete only
	// succeeds when the map still holds OUR observed inf pointer; if a
	// fresh entry was stored it leaves the new one alone.
	//
	// R040034-CHANGES (#1416 review) — KNOWN narrow remaining window:
	// CompareAndDelete-on-pointer closes Load+CompareAndDelete TOCTOU
	// for the case where the map has already been swapped to a fresh
	// *runInflight by a racing AddJob+jobInflight. It does NOT close the
	// adjacent window where executeOpt has already done
	//   inflight := s.jobInflight(j.ID)          // gets old *runInflight
	// at scheduler_run.go ~line 867 just before this cleanup runs;
	// cleanup then deletes the map entry + the still-CAS=false old gate
	// (releaseRun has executed), and executeOpt's CompareAndSwap on
	// the orphaned old gate succeeds. A second executeOpt for the same
	// jobID will then LoadOrStore a fresh *runInflight via jobInflight
	// and CAS-win on it too → two goroutines hold distinct gates for
	// one jobID. We accept this remaining window because it requires:
	//   (i)  DeleteJob racing TriggerNow on the same job ID, AND
	//   (ii) ID reuse on a fresh AddJob landing within microseconds,
	// and crypto/rand 8-byte (2^64) ID space makes (ii) ~2^-32 over
	// the entire process lifetime at maxJobsHardCap=500 working set.
	// A future tightening would add a per-jobID lock around the
	// jobInflight Load → CAS pair in executeOpt; deferred until
	// telemetry indicates the window is operationally reachable.
	s.runningJobs.CompareAndDelete(jobID, inf)
	return true
}

// jobInflight returns a lazily created *runInflight per job ID. The
// embedded atomic.Bool keeps the original CAS-gate semantics (used by
// executeOpt to reject concurrent runs); the surrounding metadata fields
// expose RunID/StartedAt/Phase to the list API for the cron-run-history
// P0 visibility work.
//
// Entries are reclaimed on DeleteJob via cleanupRunningJobIfIdle when
// the CAS gate is idle (R242-ARCH-15 / #758). The prior never-cleanup
// policy was a worst-case bound of maxJobsHardCap=500 entries; in long-
// lived deployments that delete and re-add jobs the working set could
// grow without limit.
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

// rangeRunningSessionIDs invokes fn for the Claude session ID of every
// currently-running inflight run (a run whose SessionID has been populated by
// setSessionID after GetOrCreate). fn returning false stops the iteration
// early — like sync.Map.Range — so a caller searching for one ID can bail on
// the first hit. Empty SessionIDs (run started but session not yet minted)
// and non-running snapshots are skipped before fn sees them.
//
// R249-CR-4 / R260528-ARCH-7 (#948 / #1368): containsSessionID and
// buildKnownSessionsSet both open-coded the s.runningJobs.Range +
// *runInflight type-assert + snapshot + running/non-empty guard. Folding the
// boilerplate here decouples both callers from the s.runningJobs sync.Map
// representation (one of the fields the god-struct issue flags) and keeps the
// inflight-view contract in a single place.
func (s *Scheduler) rangeRunningSessionIDs(fn func(sessionID string) bool) {
	s.runningJobs.Range(func(_, v any) bool {
		inf, ok := v.(*runInflight)
		if !ok || inf == nil {
			return true
		}
		view, running := inf.snapshot()
		if !running || view.SessionID == "" {
			return true
		}
		return fn(view.SessionID)
	})
}

// jobSnapshot captures the mutable Job fields executeOpt reads under s.mu so
// the long-running send/notify pipeline can run without holding the lock.
// Snapshot is taken once after the rate-limit/jitter gate and reused for the
// rest of the execution; concurrent SetJobPrompt/UpdateJob therefore land
// for the next tick rather than racing the in-flight result. The shape
// mirrors the original inline reads — no fields added/removed.
//
// R247-CR-16: 字段按 size DESC 排，消除 string/bool/*bool 混排引入的 padding。
type jobSnapshot struct {
	prompt  string
	workDir string
	jobID   string
	// label is the human-readable title for IM notice prefixes (R233B-CR-4 /
	// R233B-CR-5). Computed via jobTitleOrFallback under s.mu so a
	// concurrent SetJobPrompt cannot tear Title vs Prompt-derived fallback.
	// Empty when both Title and Prompt are blank — deliverNotice falls back
	// to jobID in that case so the IM prefix never collapses to "[Cron ]".
	label      string
	platName   string
	chatID     string
	notifyPlat string
	notifyChat string
	schedule   string
	backend    string // "" = router default
	// lastSessionID 是 snapshot 时刻 Job.LastSessionID 的拷贝，供 fresh-
	// preflight 的 stub-refresh 闭包使用。R239-PERF-13: 闭包以前在每次
	// 失败回调时再开 s.mu.RLock 读 s.jobs[jobID].LastSessionID，新增本字段
	// 后 refresh 可直接调 registerStubByValue 不再触锁。语义保留——失败路径
	// 用 snap-time chain anchor（与本次 attempt 起点一致），后续新成功 run
	// 由其 finishRun 路径再覆写。
	lastSessionID string
	notify        *bool // nil = unset
	fresh         bool
}

// cronNoticePrefixFmt is the IM-notice prefix template every cron-side
// deliverNotice call funnels through. Centralising the literal closes
// R247-CR-5 (REPEAT-3): three execute-path notice strings each carried
// their own copy of "[Cron %s] …", so the only thing pinning the prefix
// shape was test fixtures grepping the formatted string. New notice
// sites should compose via formatCronNotice rather than inline a 4th
// copy.
//
// R247-PERF-7 (#539): the formatter implementation uses strings.Builder
// instead of fmt.Sprintf so a busy scheduler firing N notices/min stops
// paying the reflection-driven %s walk on already-bounded label/body
// strings. The template literal stays for documentation / test fixture
// grep — the formatter inlines its shape ("[Cron <label>] <body>") so a
// future template change has to touch both sides, which the
// notice_label_bracket_test invariants pin.
const cronNoticePrefixFmt = "[Cron %s] %s"

// cronNoticePrefix / cronNoticeMid / cronNoticeSuffix are the literal
// segments stitched into formatCronNotice's strings.Builder output. They
// are intentionally separate consts so a future template change cannot
// silently desync from cronNoticePrefixFmt above (the
// notice_label_bracket_test pins the exact byte sequence, which catches
// any drift).
const (
	cronNoticePrefix = "[Cron "
	cronNoticeMid    = "] "
)

// cronMarkdownPunctReplacer is a package-level Replacer for escapeCronMarkdownPunct.
// Constructed once to avoid per-call allocation; Replace performs a single
// pass over the input string. R164930-PERF-4.
var cronMarkdownPunctReplacer = strings.NewReplacer(
	"[", "［", // U+FF3B
	"]", "］", // U+FF3D
	"(", "（", // U+FF08
	")", "）", // U+FF09
)

// formatCronNotice renders the IM-notice line cron jobs send through
// deliverNotice. label is the snap.labelOrID() result (job title or
// fallback ID); body is the human-readable suffix already in the
// caller's display locale (Chinese for the static error templates,
// sanitised result text on the success path). Kept as a pure formatter
// so it can be reused from non-execute code paths (e.g. future manual
// retry surface) without dragging the deliverNotice / Scheduler
// dependencies along.
//
// R239-SEC-5: label flows through to the IM channel without ever
// transiting sanitiseRunResult, so an attacker-supplied job Title (e.g.
// "‮…" RLO) — which Scheduler.AddJob's MaxCronTitleLen check does
// not strip — would land verbatim in the IM render and reverse the
// surrounding text. Force it through osutil.SanitizeForLog (covers C0/C1,
// bidi overrides + isolates, LS/PS) so the rendered notice cannot be
// hijacked by control runes hidden in the title or prompt-derived
// fallback. body is already SanitizeForLog'd on the success path
// (sanitiseRunResult); applying it here is idempotent on clean ASCII
// templates and adds defence-in-depth.
func formatCronNotice(label, body string) string {
	// MaxCronTitleLen (256 runes) bounds label after the rune-count gate
	// at AddJob/UpdateJob — a 4× rune→byte budget is more than enough for
	// CJK / emoji to round-trip through SanitizeForLog without truncation.
	label = osutil.SanitizeForLog(label, MaxCronTitleLen*4)
	// R250-SEC-6 (#1095) + R260528-SEC-8: replace markdown link-syntax
	// characters `[` `]` `(` `)` in label and body with full-width
	// visually-similar codepoints (U+FF3B / U+FF3D / U+FF08 / U+FF09).
	// This prevents an attacker-controlled Title or result body from
	// smuggling `[text](url)` clickable links into IM notices.
	// validateCronTitle blocks bidi / C0 controls but ASCII punctuation
	// passes through, so the substitution here is the safety bottom line.
	// R164930-PERF-4/5: escapeCronMarkdownPunct now performs a single-pass
	// Replacer with an IndexAny fast-path; the redundant pre-scan for `]`
	// that previously ran at this call site has been removed.
	label = escapeCronMarkdownPunct(label)
	body = escapeCronMarkdownPunct(body)
	// R247-PERF-7 (#539): strings.Builder skips fmt.Sprintf's reflection
	// walk over the already-bounded label/body inputs. Pre-grow once so
	// the underlying buffer covers the largest plausible payload (label is
	// MaxCronTitleLen runes after SanitizeForLog; body is sanitiseRunResult
	// output bounded by MaxCronResultBytes). On a busy scheduler this
	// drops one alloc + the reflect.Value boxing per notice.
	var b strings.Builder
	b.Grow(len(cronNoticePrefix) + len(label) + len(cronNoticeMid) + len(body))
	b.WriteString(cronNoticePrefix)
	b.WriteString(label)
	b.WriteString(cronNoticeMid)
	b.WriteString(body)
	return b.String()
}

// escapeCronMarkdownPunct replaces the markdown link-syntax characters
// `[`, `]`, `(`, `)` with full-width visually-similar codepoints
// (U+FF3B / U+FF3D / U+FF08 / U+FF09) so an attacker-controlled cron
// Title or result body cannot smuggle `[text](url)` clickable links
// into the IM notice. R260528-SEC-8.
//
// R164930-PERF-4/5: single-pass implementation using a package-level
// strings.Replacer (cronMarkdownPunctReplacer). An IndexAny fast-path
// avoids any allocation on the common ASCII-clean case; when substitution
// is required the Replacer performs exactly one scan + one output
// allocation instead of the previous up-to-4-scan / 4-alloc loop.
func escapeCronMarkdownPunct(s string) string {
	if !strings.ContainsAny(s, "[]()") {
		return s
	}
	return cronMarkdownPunctReplacer.Replace(s)
}

// EscapeMarkdownPunct is the exported variant of escapeCronMarkdownPunct for
// use by packages (e.g. dispatch) that display cron Job fields in IM replies.
// R112714-ARCH-1.
func EscapeMarkdownPunct(s string) string { return escapeCronMarkdownPunct(s) }

// labelOrID returns the IM-notice display label: snap.label when populated,
// jobID otherwise. R233B-CR-5: keeps the "[Cron <X>] …" prefix readable
// without crashing on Title-empty + Prompt-empty edge cases.
func (s jobSnapshot) labelOrID() string {
	if s.label != "" {
		return s.label
	}
	return s.jobID
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
	return snapshotJobLocked(j)
}

// snapshotJobLocked is the lock-held variant of snapshotJob: callers MUST
// hold s.mu (read or write). Used by executeOpt's jitter-window block to
// fold the post-jitter `cur.Paused` recheck and the snapshot copy into a
// single RLock window — eliminates the back-to-back RLock pair flagged in
// R20260528-PERF-2 (#1351). Kept as a free function (rather than a method
// on *Scheduler) so the dependency on the caller's RLock is documented at
// the call site and the helper cannot accidentally re-acquire the lock.
func snapshotJobLocked(j *Job) jobSnapshot {
	snap := jobSnapshot{
		prompt:        j.Prompt,
		workDir:       j.WorkDir,
		jobID:         j.ID,
		label:         jobTitleOrFallback(j),
		platName:      j.Platform,
		chatID:        j.ChatID,
		notifyPlat:    j.NotifyPlatform,
		notifyChat:    j.NotifyChatID,
		fresh:         j.FreshContext,
		schedule:      j.Schedule,
		backend:       j.Backend,
		lastSessionID: j.LastSessionID,
	}
	if j.Notify != nil {
		v := *j.Notify
		snap.notify = &v
	}
	return snap
}

// preflightArgs bundles the inputs to freshContextPreflightP0. R229-CR-8.
// Mirrors finishArgs's struct-bag pattern: the helper has 8 inputs that all
// flow through to the same finishRun/deliverNotice call sites and keeping
// them as positional args made future additions (e.g. a new error-class)
// risk silent argument-order swaps. Named fields also let tests express
// intent without reading parameter positions.
//
// R246-CR-014 (#757): field ordering is size DESC so the value type packs
// without intra-struct padding.
//
//	snap (jobSnapshot, ~144B incl. *Job/strings/bools — already size-DESC'd)
//	startedAt (time.Time, 24B)
//	notifyTo (NotifyTarget, 32B but two strings — keep with strings group)
//	16B fields (key, runID, trigger=string-typed) grouped
//	8B pointers (job, lg) trailing
//
// Pre-fix order (job → snap → key → lg → notifyTo → runID → startedAt →
// trigger) interleaved 8-byte pointers and 16-byte strings; on amd64/arm64
// each layout-mismatched neighbour added 8 bytes of padding, wasting
// ~16 bytes per executeOpt call (~1Hz × N jobs lifetime).
type preflightArgs struct {
	// snap 是 snapshotJob 拷贝出的快照（fresh / workDir / prompt /
	// jobID / labelOrID）。preflight 优先读 snap 而非 *job，避免与并发
	// DeleteJob/PauseJob 起读写竞争。Largest field — leads the struct so
	// no 8B head pads it down to a 16B / 24B boundary.
	snap jobSnapshot
	// startedAt 是 caller 进入 executeOpt 时记录的 wall-clock 起点；
	// finishRun 据此算 durationMS。preflight 失败也保留这个起点而非
	// 重新 time.Now()，让 dashboard 看到真实的"从触发到放弃"时长。
	startedAt time.Time
	// notifyTo 是 fresh-preflight 工作目录不可达分支用来回写
	// 「[Cron …] 工作目录不可达」中文提示的目标；其它失败分支不通知，
	// 因为「shutdown / Reset 失败」对终端用户没有可操作信号。Two strings
	// = 32B; sits with the 16B string-shaped fields below.
	notifyTo NotifyTarget
	// key 是 router GetOrCreate / Reset 用到的 session key
	// （`cron:<jobID>` 形式）。fresh 路径 Reset 该 key 后再让 caller
	// 重新 GetOrCreate，确保新 CLI 进程接管。
	key string
	// runID 是 caller 已生成的 16-char hex 运行 ID。失败分支转给
	// finishRun，使 cron_run_ended 与 cron_run_started 配对（emitOverlapSkipped
	// 同样模式）。
	runID string
	// trigger 区分 TriggerScheduled / TriggerManual；deliverNotice 与
	// dashboard run timeline 对二者渲染不同图标。Underlying type string
	// (16B) — packs with key/runID.
	trigger TriggerKind
	// job 是 freshContextPreflightP0 操作的目标 Job 指针（持有用于
	// stub-refresh 闭包），调用前 caller 已 snapshot；preflight 不会修改
	// *Job 字段，但失败分支会通过 finishArgs.job 把它转交给 finishRun。
	// 8B trailing — pointers grouped at the end.
	job *Job
	// lg 是带 jobID/runID 标签的 slog.Logger，preflight 自身只输出
	// info/warn 不输出 error（error 由 finishRun 的 errMsg 落盘统一处理）。
	lg *slog.Logger
	// finalizer 是 caller 栈上的 *runFinalizer。preflight 失败分支把它转
	// 交给 finishRun，让 cron_run_ended broadcast 之前 finalize 元数据，
	// CurrentRun(jobID) 与 broadcast 同步可见 ok=false。R246-GO-3 (#689).
	finalizer *runFinalizer
}

// stubRefresher carries the snap-time chain anchor (jobID + workDir + prompt
// + lastSessionID) that the error-path sidebar re-registration needs.
// R249-ARCH-25 (#989): freshContextPreflightP0 previously returned a bare
// `func()` closure that implicitly captured `snap`; the closure's lifetime
// and exactly which snap fields it pinned were invisible at the call site.
// Promoting it to a typed value with an explicit field set makes the captured
// state auditable (the four fields below are the entire dependency surface)
// and the zero value is a safe no-op — run() short-circuits when active is
// false, so the persistent-mode / early-bail paths need no special-casing.
//
// Unlike the R232-CR-7 single-field preflightResult wrapper that was removed,
// this struct carries the operation's actual value payload (not a lone func
// field), so it does not reintroduce that anti-pattern.
type stubRefresher struct {
	s             *Scheduler
	jobID         string
	workDir       string
	prompt        string
	lastSessionID string
	active        bool
}

// run re-registers the sidebar stub for the snapshotted job iff it still
// exists. The zero value (active=false) is an intentional no-op so callers
// invoke run() uniformly after both success-short-circuit and failure
// branches. stillExists is re-checked under s.mu because the failure callback
// may fire seconds after preflight returned, by which point DeleteJob could
// have removed the job — re-registering a stub for a deleted job would leak a
// phantom sidebar row. See the lock-pair contract at freshContextPreflightP0.
func (r stubRefresher) run() {
	if !r.active {
		return
	}
	r.s.mu.RLock()
	_, exists := r.s.jobs[r.jobID]
	r.s.mu.RUnlock()
	if exists {
		r.s.registerStubByValue(r.jobID, r.workDir, r.prompt, r.lastSessionID)
	}
}

// freshContextPreflightP0 handles the fresh-mode prologue: ctx-cancel guard
// (CRON3), work-dir reachability check (CRON2), Reset, and the post-Reset
// existence re-check that prevents a leaked CLI process tied to a deleted
// job ("cron:<id>" orphan). Each failure branch records a (RunState,
// ErrorClass) tuple via finishRun so the run-history terminal protocol
// (broadcast cron_run_ended + counters + LastErrorClass write) participates.
//
// Returns:
//   - stubRefresh: closure that re-registers the sidebar stub on error
//     paths so the cron row stays visible. Caller invokes after error
//     branches; never invoke on success (live session owns the row).
//   - ok: false means the caller MUST return immediately. The helper has
//     already written the appropriate slog.Info/Warn + finishRun() for
//     the failure mode.
//
// In persistent mode (snap.fresh=false) the helper short-circuits with
// ok=true and a no-op stubRefresh so the caller's flow is uniform.
//
// R232-CR-7：原 preflightResult{stubRefresh: ...} 单字段 wrapper struct
// 已删除，直接返回二元组。
func (s *Scheduler) freshContextPreflightP0(args preflightArgs) (stubRefresh stubRefresher, ok bool) {
	snap := args.snap
	lg := args.lg
	noopRefresh := stubRefresher{} // active=false → run() is a no-op
	if !snap.fresh {
		return noopRefresh, true
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
			finalizer: args.finalizer,
		})
		return noopRefresh, false
	}
	if !workDirReachable(snap.workDir) {
		lg.Warn("cron fresh spawn aborted: work_dir unreachable",
			"work_dir", snap.workDir)
		s.finishRun(finishArgs{
			job: args.job, runID: args.runID, startedAt: args.startedAt, trigger: args.trigger,
			state: RunStateFailed, errClass: ErrClassWorkDirUnreachable,
			errMsg: "work_dir unreachable",
			prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
			finalizer: args.finalizer,
		})
		s.deliverNotice(args.notifyTo, formatCronNotice(snap.labelOrID(), "工作目录不可达，本次执行已跳过。"))
		return noopRefresh, false
	}
	// R20260603140013-CR-3: containment early-check BEFORE the destructive
	// Reset below. resolveCronWorkspace (executeOpt) already aborts outside-root
	// runs, but it uses the TTL-cached workDirResolveUnderRootCached view; a
	// symlink retargeted outside allowedRoot within that TTL would pass there as
	// a stale-positive, letting us reach this point and blow away a live session
	// (Reset destroys the cron:<jobID> session + its process + history) for a
	// run that can never succeed. Re-validate with the uncached workDirUnderRoot
	// here so a freshly-retargeted symlink fails the run WITHOUT tearing down the
	// existing session. Mirrors resolveCronWorkspace's outside-root finishRun
	// (RunStateFailed / ErrClassWorkDirOutsideRoot) and the workDirReachable
	// branch's deliverNotice. noopRefresh leaves the sidebar stub untouched.
	if s.allowedRoot != "" && snap.workDir != "" &&
		!workDirUnderRoot(snap.workDir, s.allowedRoot, s.allowedRootResolved) {
		lg.Warn("cron fresh spawn aborted: work_dir outside allowed root",
			"work_dir", snap.workDir)
		s.finishRun(finishArgs{
			job: args.job, runID: args.runID, startedAt: args.startedAt, trigger: args.trigger,
			state: RunStateFailed, errClass: ErrClassWorkDirOutsideRoot,
			errMsg: "work_dir outside allowed root",
			prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
			finalizer: args.finalizer,
		})
		s.deliverNotice(args.notifyTo, formatCronNotice(snap.labelOrID(), "工作目录超出允许根目录，本次执行已跳过。"))
		return noopRefresh, false
	}
	// CRON1 / R194 (#401) — fresh-context atomicity invariant.
	//
	// Reset(key) here and the caller's subsequent GetOrCreate(key) (executeOpt
	// line ~1321) are two separate s.router (r.mu) acquisitions. A concurrent
	// rebuild landing in the gap could resurrect the cron:<jobID> session with
	// stale opts, bypassing fresh semantics. The concrete *session.Router DOES
	// expose an atomic primitive (ResetAndRecreate, router_lifecycle.go) but
	// the cron consumer interface (SessionRouter, scheduler.go) deliberately
	// does not surface it — correctness instead rests on a documented
	// single-writer invariant rather than a lock:
	//
	//   (1) cron↔cron: executeOpt is serialized per jobID by the inflight CAS
	//       gate (inflight.running.CompareAndSwap, executeOpt line ~947). A
	//       scheduled tick and a concurrent TriggerNow for the SAME job cannot
	//       both reach this Reset — the loser short-circuits at the CAS. This
	//       half is enforced in-package and pinned by the contract test in
	//       fresh_context_reset_atomic_test.go.
	//
	//   (2) cron↔external: the cron:<jobID> session-key namespace
	//       (sessionkey.CronKey) is reserved for the scheduler. Dashboard /
	//       IM sends route to channel-scoped keys (feishu:..., dashboard:...),
	//       never to a cron: key, so no external GetOrCreate races this Reset.
	//
	// If a future feature lets users send directly into a cron:<jobID>
	// session, invariant (2) breaks and this MUST migrate to the existing
	// router-level ResetAndRecreate primitive by adding it to the SessionRouter
	// interface and switching the preflight here — not by authoring a new one
	// (issue #401 Option A).
	s.router.Reset(args.key)
	lg.Info("cron fresh context: session reset before run")
	// R239-PERF-13: refresh 闭包改用 snap 固化值直接调 registerStubByValue，
	// 不再每次失败回调时重开 s.mu.RLock 读 s.jobs[jobID]。snap 由
	// snapshotJob 在 RLock 下一次性拷贝（包括 LastSessionID），失败路径
	// 用这份 snap-time chain anchor 即可，后续新成功 run 由其 finishRun
	// 写新 LastSessionID 并由下一轮 snap 自然带入；闭包路径只是兜底让
	// sidebar 在失败后仍能渲染。仍需走 stillExists 校验：job 可能在
	// Reset 与本回调间隔内被 DeleteJob 删掉，那种情况下 stub 不应再注册。
	//
	// R20260527-COR-12 (#1298) lock-pair contract：本函数对 s.jobs[snap.jobID]
	// 做两次 RLock 读，对应两个时间点：
	//
	//   (a) refresh closure 内（line ~490）：失败回调晚于本函数返回，闭包
	//       可能在数秒后才执行；那时 job 是否仍存在必须重读。
	//   (b) 紧随 Reset 之后（line ~497）：post-Reset 防御 — Reset 已经清空
	//       sessions/<key> 会话状态；如果 job 此刻已被 DeleteJob 删掉，
	//       本函数必须返回 ok=false 防止后续 GetOrCreate 重建一个 cron:<id>
	//       孤儿。
	//
	// 两次读独立、各自的 RLock 持锁窗口短小，且(a)只在(b)成功后才有机会
	// 触发，所以"重复读 snap.jobID"是设计意图而非 bug。Reviewer 看到第二
	// 次 RLock 时不要"合并优化"——会让(a)失去独立的 stillExists 检查。
	refresh := stubRefresher{
		s:             s,
		jobID:         snap.jobID,
		workDir:       snap.workDir,
		prompt:        snap.prompt,
		lastSessionID: snap.lastSessionID,
		active:        true,
	}
	// (b) post-Reset 存在性检查 — 见上文 lock-pair contract。
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
			finalizer: args.finalizer,
		})
		return refresh, false
	}
	return refresh, true
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
// abortResult bundles the watchdog's exit signal: whether it actually
// fired the interrupt (i.e. the ctx ended via DeadlineExceeded, not via
// success-path Cancel) and what the InterruptViaControl outcome was when
// it did. The fired flag is the discriminator the caller logs.
//
// outcome is the cron-local InterruptOutcome; the production adapter
// in cmd/naozhi/cron_router_adapter.go casts session.InterruptOutcome
// → cron.InterruptOutcome via a numeric cast, with an init() panic
// pinning the ordinals.
//
// R238-ARCH-20 (#787) proposed renaming deadlineInterrupter →
// RunInterrupter and switching abortResult to a fresh InterruptResult
// enum to "break the dependency on session.InterruptOutcome". The
// decoupling is already complete: the cron-local InterruptOutcome above
// (defined in agent_opts.go) is the public type; cron does NOT import
// session.InterruptOutcome anywhere in production code (the last
// reverse import was eliminated by R20260527122801-ARCH-1, see the
// banner in scheduler_session.go). The proposed rename is a cosmetic
// preference rather than an architectural fix; deferring keeps the
// type name aligned with the concept "deadline-driven interrupt"
// across godoc / metrics / tests, and avoids a sweep across N test
// files. The fired-vs-success ambiguity flagged in the issue is
// addressed by abortResult.fired's godoc above (success path is
// fired=false; only the watchdog firing sets fired=true).
type abortResult struct {
	outcome InterruptOutcome
	fired   bool
}

// deadlineInterrupter is the narrow capability runDeadlineWatchdog needs
// from a session: a way to abort an in-flight CLI turn via the protocol's
// control_request channel. cron.Session satisfies this; cron tests
// stub it with a counting mock to assert the watchdog fired exactly when
// the deadline elapsed without having to also implement Send.
type deadlineInterrupter interface {
	InterruptViaControl() InterruptOutcome
}

// watchdogInterruptTimeoutDefault caps how long runDeadlineWatchdog
// will wait for InterruptViaControl to return before recording the
// attempt as InterruptError and unblocking the caller. R236-GO-09 (#507):
// pre-fix, a wedged session.InterruptViaControl (control_request channel
// pinned by a stuck stdin write or a kernel-blocked syscall) would hold
// the goroutine forever; the caller's `<-abortCh` then blocked forever
// and finishRun was never invoked, leaving inflight.running=true so
// every subsequent tick skipped the job until process restart. Bounding
// the call at 3s lets finishRun fire on the recovery path so the next
// tick has a chance to spawn a fresh session. The InterruptViaControl
// call itself is not aborted (no underlying ctx) — it leaks until the
// session teardown unblocks it, but the leak is bounded (per-run,
// drained on session.Reset) and far less harmful than a permanently
// stuck job.
const watchdogInterruptTimeoutDefault = 3 * time.Second

// watchdogInterruptTimeoutAtomic stores the effective timeout in
// nanoseconds. Atomic so the timeout regression tests can shorten it
// (typically to 50ms so they don't burn 3s of CI wall time) without
// racing the production read in the watchdog goroutine. Tests must
// always restore the previous value via defer.
var watchdogInterruptTimeoutAtomic atomic.Int64

// watchdogParkedInterruptGoroutines is a LIVE gauge of inner
// InterruptViaControl goroutines that outlived their watchdog after the
// interrupt-call timeout fired and are still parked on a wedged stdin
// write (R20260602-GO-005, #1632). It differs from
// metrics.CronWatchdogInterruptTimeoutTotal, which only counts cumulative
// timeout events: a persistent (non-fresh) cron job that never reaches
// session.Reset can accumulate permanently-parked goroutines, and the
// cumulative counter cannot distinguish "fired N times, all since
// drained" from "N still leaked right now". This gauge is incremented
// when the timeout branch parks the inner goroutine and decremented when
// that goroutine eventually returns (if ever), so operators can alert on
// a steadily rising live value rather than inferring it from process
// goroutine growth. expvar registration is package-global; the var stays
// in cron's file domain (no internal/metrics edit) since it observes a
// cron-internal lifecycle.
var watchdogParkedInterruptGoroutines = expvar.NewInt("naozhi_cron_watchdog_parked_interrupt_goroutines")

func init() {
	watchdogInterruptTimeoutAtomic.Store(int64(watchdogInterruptTimeoutDefault))
}

// watchdogInterruptTimeout reads the active interrupt-call timeout.
// Production callers see watchdogInterruptTimeoutDefault unless a test
// has overridden it via the atomic.
func watchdogInterruptTimeout() time.Duration {
	return time.Duration(watchdogInterruptTimeoutAtomic.Load())
}

// runDeadlineWatchdog arranges for sess.InterruptViaControl to fire
// exactly when ctx ends with DeadlineExceeded. The interrupt must run
// concurrently with sess.Send, NOT after — Send's internal defer flips
// Process.State Running→Ready the instant ctx fires, and
// InterruptViaControl gates on State==StateRunning, so calling it
// post-Send is dead code (returns ErrNoActiveTurn → outcome=no_turn).
//
// Channel contract (R249-CR-27): the returned channel has buffer=1 and
// is intentionally NOT closed. The publishing goroutine self-completes
// thanks to buffer=1 — its single send never blocks, so it returns
// regardless of whether the caller reads. The caller drains ch only to
// observe the abort outcome (abort.fired / abort.outcome) for logging
// and to ensure InterruptViaControl has finished before recording the
// run state; failing to drain leaks the abortResult value, NOT the
// goroutine, and is harmless for shutdown bookkeeping.
//
// On the success / non-deadline error path the caller cancels ctx
// explicitly; the publishing callback observes ctx.Err()==Canceled,
// skips InterruptViaControl, and returns abortResult{fired:false}.
//
// R247-GO-12 (#492): we use context.AfterFunc rather than spawning a
// long-lived `<-ctx.Done()` goroutine. With per-tick CAS-protected runs,
// a 50-job @ 1Hz deployment otherwise holds ~50 watchdog goroutines
// concurrently for the duration of every Send (up to jobTimeout). With
// AfterFunc the runtime only spawns a goroutine when ctx ends — briefly,
// to invoke the callback — so the steady-state in-flight watchdog
// goroutine count drops from O(in-flight runs) to ~0. The deadline /
// cancel semantics are preserved exactly: the callback inspects
// ctx.Err() the same way the goroutine used to.
func runDeadlineWatchdog(ctx context.Context, sess deadlineInterrupter) <-chan abortResult {
	// R249-GO-3: defensive nil guard. A nil ctx would panic on
	// context.AfterFunc; a nil sess would panic on InterruptViaControl
	// when the deadline path fires. Both are caller bugs (production wires
	// real values), but the cron run goroutine swallows panics via
	// robfig/cron's recover chain elsewhere — here a panic would surface as
	// "cron logger" Error noise without the run ever recording a result.
	// Return a pre-completed channel so the caller's `<-abortCh` sees a
	// zero abortResult and proceeds with normal finishRun bookkeeping.
	// Buffer=1 with no close mirrors the success-path contract: the caller
	// drains exactly once; an unclosed channel of buffer=1 with one send
	// already buffered satisfies that without leaking a goroutine.
	if ctx == nil || sess == nil {
		ch := make(chan abortResult, 1)
		ch <- abortResult{}
		return ch
	}
	ch := make(chan abortResult, 1)
	context.AfterFunc(ctx, func() {
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			ch <- abortResult{}
			return
		}
		// R236-GO-09 (#507) / R247-GO-5 (#476) / R246-GO-6: InterruptViaControl
		// can block indefinitely when the protocol channel is wedged
		// (kernel-blocked stdin write, control_request never acked). Bound
		// it so the caller always observes a result on abortCh and
		// finishRun runs — otherwise inflight.running stays true and every
		// subsequent tick silently skips the job AND the wrapper goroutine
		// holds the abortCh slot past Stop's stopBudget during scheduler
		// shutdown (the systemd TimeoutStopSec failure mode the R247-GO-5
		// anchor explicitly cited). The done channel is buffered=1 so the
		// inner goroutine never blocks on send: it returns whenever
		// InterruptViaControl finishes, even after the timeout branch has
		// already published an InterruptError outcome.
		done := make(chan InterruptOutcome, 1)
		// state coordinates the live leak-gauge accounting between this
		// inner goroutine and the timeout branch below (R20260602-GO-005,
		// #1632). It is a 3-state CAS race resolver:
		//   0 = neither side has acted yet
		//   1 = inner goroutine returned first (watchdog must NOT park it)
		//   2 = watchdog fired first and parked the inner goroutine
		//       (gauge incremented; inner goroutine must decrement on exit)
		// Exactly one of the two CAS(0→1)/CAS(0→2) wins, so the gauge is
		// incremented and later decremented at most once per park — no
		// leak under any interleaving of "inner returns" vs "watchdog
		// fires".
		var state atomic.Int32
		go func() {
			outcome := sess.InterruptViaControl()
			if !state.CompareAndSwap(0, 1) {
				// Lost the race: watchdog already parked us (state==2) and
				// incremented the gauge. We outlived the watchdog but the
				// wedged write finally unblocked — undo the increment.
				watchdogParkedInterruptGoroutines.Add(-1)
			}
			done <- outcome
		}()
		// R20260527122801-GO-001: NewTimer + defer Stop mirrors
		// scheduler.go:1337 — time.After leaks a *Timer slot until
		// expiry on the success path.
		t := time.NewTimer(watchdogInterruptTimeout())
		defer t.Stop()
		select {
		case outcome := <-done:
			ch <- abortResult{outcome: outcome, fired: true}
		case <-t.C:
			// R20260527122801-SEC-3 (#1327): the inner goroutine above
			// is still parked on InterruptViaControl and will outlive
			// this watchdog goroutine until the wedged stdin write
			// unblocks (typically on the next session.Reset; for non-
			// fresh jobs that may never happen on its own). Surface the
			// event via a metric + Warn log so operators can alert on
			// rising deltas rather than discovering it via slow goroutine
			// growth. The metric lives next to other cron counters in
			// internal/metrics so the dashboard wireup is identical.
			metrics.CronWatchdogInterruptTimeoutTotal.Add(1)
			// R20260602-GO-005 (#1632): record the parked goroutine on a
			// LIVE gauge so a persistent (never-reset) job's permanent
			// leak is observable as a rising current count, not just a
			// cumulative timeout total. CAS(0→2) only wins if the inner
			// goroutine has not already returned; if it lost the race the
			// goroutine is gone and there is nothing to count. The matching
			// Add(-1) lives in the inner goroutine's lost-race branch.
			if state.CompareAndSwap(0, 2) {
				watchdogParkedInterruptGoroutines.Add(1)
			}
			slog.Warn("cron watchdog: InterruptViaControl timeout exceeded; inner goroutine parked until session reset",
				"timeout", watchdogInterruptTimeout())
			ch <- abortResult{outcome: InterruptError, fired: true}
		}
	})
	return ch
}

// sendWithWatchdog runs sess.Send under a deadline-watchdog and returns
// the SendResult, the watchdog abortResult, and the Send error in one
// shot. R215-ARCH-P2-5 (#581) partial: factored out of executeOpt so
// the four-step invariant — (1) start watchdog, (2) Send, (3)
// sendCancel so the watchdog returns on the success path, (4) drain
// abortCh BEFORE the next session.Reset to avoid the in-flight
// interrupt write racing the next tick — lives in one named function
// instead of inlined in a 569-line state machine where a future split
// could accidentally reorder the cancel/drain pair.
//
// Caller contract:
//   - sendCtx must be a context.WithTimeout / s.stopCtx-derived ctx.
//     Watchdog uses ctx.Err() == DeadlineExceeded as its fire trigger;
//     Background or any non-deadline ctx degrades to "interrupt never
//     fires" silently.
//   - sendCancel is called by this helper exactly once after Send
//     returns; the caller's `defer sendCancel()` is therefore a no-op
//     (cancelFunc is idempotent).
func sendWithWatchdog(sendCtx context.Context, sendCancel context.CancelFunc, sess Session, text string) (SendResult, abortResult, error) {
	// Watchdog: deadline-fired interrupt of the in-flight CLI turn. See
	// runDeadlineWatchdog for the rationale (must fire BEFORE Send
	// returns, otherwise Process.State has already flipped to Ready and
	// InterruptViaControl returns ErrNoActiveTurn → no-op).
	abortCh := runDeadlineWatchdog(sendCtx, sess)

	// Direct Send without sendWithBroadcast — cron jobs notify via the
	// IM deliverNotice path (resolveNotifyTarget + platform.Reply) and
	// the cron_run_ended WS frame.
	result, err := sess.Send(sendCtx, text)

	// Cancel sendCtx so the watchdog returns promptly on the success /
	// non-deadline error path; on the deadline path it's already done.
	// Block on abortCh so the InterruptViaControl call (if any)
	// completes before we record the run state — otherwise a fast cron
	// tick could overlap the next session.Reset with the in-flight
	// interrupt write.
	sendCancel()
	abort := <-abortCh
	return result, abort, err
}

// classifyExecError maps an error from GetOrCreate or Send to
// (RunState, ErrorClass) for finishRun. defaultClass distinguishes the
// session-spawn path (ErrClassSessionError) from the send path
// (ErrClassSendError); the helper unconditionally remaps the two
// context-derived sentinels:
//
//   - context.DeadlineExceeded → (RunStateTimedOut, ErrClassDeadlineExceeded)
//   - context.Canceled         → (RunStateCanceled, ErrClassCanceled)
//
// R241-ARCH-7: Canceled was historically handled by the caller via a
// dedicated `if errors.Is(err, context.Canceled)` branch ahead of this
// helper, so the state mapping was split across this site (DeadlineExceeded
// only) and the two caller blocks (Canceled / default). Folding Canceled
// into the helper keeps all (err → state, errClass) decisions in one
// place. Callers still own the side-effects that DIFFER per class
// (skipPersist=true for Canceled, operator-facing notice suppressed for
// Canceled, abort.fired logging on the send path) — see executeOpt's
// switch on errClass below for those policy choices.
//
// errors.Is order matters: context.Canceled wraps both genuine
// cancellation AND the "parent ctx cancelled mid-DeadlineExceeded" race
// where Send returns context.Canceled even though the deadline ticked
// first. Checking DeadlineExceeded first preserves the historical
// classification (deadline-exceeded WINS) so jobs that hit jobTimeout
// during a graceful shutdown still record RunStateTimedOut rather than
// RunStateCanceled. R230C-CR-7 (original) + R241-ARCH-7 (Canceled fold).
func classifyExecError(err error, defaultClass ErrorClass) (RunState, ErrorClass) {
	if errors.Is(err, context.DeadlineExceeded) {
		return RunStateTimedOut, ErrClassDeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return RunStateCanceled, ErrClassCanceled
	}
	return RunStateFailed, defaultClass
}

// applyJitterAndRecheck performs the post-CAS jitter sleep and the
// post-jitter delete/pause recheck for a scheduled (non-TriggerNow) run with
// jitter enabled. Extracted verbatim from executeOpt under R249-CR-1 (#945) /
// R238-ARCH-2 (#734) so the run path reads as a sequence of named phases
// rather than one ~340-line state machine; behaviour is unchanged.
//
// Returns:
//   - snap / snapTaken: when the recheck passes, snap is the under-RLock
//     snapshot of j and snapTaken is true so the caller skips the redundant
//     fall-through snapshotJob. On the abort paths snapTaken is false.
//   - abort: true means a DeleteJob / PauseJobByID landed during the jitter
//     window; the caller MUST return immediately (the deferred finalizer
//     releases the inflight CAS + gauge). The aborting slog.Debug is emitted
//     here so the caller's branch stays a bare `return`.
//
// Caller contract: only invoked when !viaTriggerNow && s.jitterMax > 0, and
// after inflight metadata is populated (so setPhase(PhaseJittering) is the
// correct transition).
func (s *Scheduler) applyJitterAndRecheck(j *Job, runID string, inflight *runInflight) (snap jobSnapshot, snapTaken bool, abort bool) {
	inflight.setPhase(PhaseJittering)
	// R250-GO-1: snapshot Schedule under s.mu.RLock so a concurrent
	// UpdateJob mutating j.Schedule doesn't race with applyJitter's
	// read. Mirrors the pattern used for the cur.Paused check below.
	//
	// R250-CR-14 (#1147): also snapshot j.entryID so we can fetch the
	// already-parsed robfigcron.Schedule via s.cron.Entry(entryID)
	// instead of re-parsing the schedule string inside applyJitter.
	// cronParser.Parse uses regex + struct alloc; on every tick of every
	// jittered job this was wasted work since robfig/cron already holds
	// the parsed Schedule for dispatch. Fall back to the string-parse
	// path (applyJitter) if entryID is 0 (job not yet registered, e.g.
	// tests) or if the entry has been removed concurrently (DeleteJob
	// races) — the parse-fallback preserves the historical behaviour.
	s.mu.RLock()
	schedStr := j.Schedule
	entryID := j.entryID
	cachedPeriod := j.cachedPeriod
	var parsedSched robfigcron.Schedule
	if entryID != 0 && cachedPeriod <= 0 {
		// R242-PERF-2 (#664): only fetch the parsed Schedule for the live
		// computation when the cache is cold. registerJob populates
		// cachedPeriod alongside entryID, so production runs hit the
		// pre-computed branch and skip both the s.cron.Entry RLock-friendly
		// lookup and the 2× sched.Next that schedulePeriodFromSched runs.
		parsedSched = s.cron.Entry(entryID).Schedule
	}
	s.mu.RUnlock()
	switch {
	case cachedPeriod > 0:
		// R242-PERF-2 (#664): hot path — period was cached at registerJob
		// time, no per-tick parsing or sched.Next needed.
		jitterSleep(s.stopCtx, cachedPeriod, s.jitterMax)
	case parsedSched != nil:
		applyJitterSched(s.stopCtx, parsedSched, s.jitterMax)
	default:
		applyJitter(s.stopCtx, schedStr, s.jitterMax)
	}

	// R220-GO-3 + R246-GO-7: a DeleteJob OR a PauseJobByID that lands
	// during the jitter window must abort the run before we spawn /
	// send. The registerJob closure has a paused-check upstream of
	// executeOpt, but it runs *before* the jitter wait — a Pause that
	// lands inside the (default up-to-30s) jitter window would
	// otherwise leak through and violate the "Paused job must not run"
	// invariant. DeleteJob also leaves the inflight CAS still held
	// until we finish — blocking TriggerNow for the same id with an
	// "already running" overlap skip; the early return below releases
	// it via the deferred inflight.running.Store(false) above.
	// snapshotJob reads under s.mu so a stale dereference is
	// impossible after Delete (the field reads return the last-known
	// values and we never use them past this point).
	//
	// R20260528-PERF-2 (#1351): the snapshot copy is also taken under
	// the SAME RLock when the recheck passes — see snapshotJobLocked.
	// This eliminates the immediately-following second RLock the
	// pre-fix code paid via s.snapshotJob(j). The
	// `cur, stillRegistered` / `paused := ...` literal pattern stays
	// in this scope so
	// TestExecuteOpt_JitterPausedReCheck_SourceAnchor in jitter_test.go
	// continues to lock down the recheck against silent removal.
	s.mu.RLock()
	cur, stillRegistered := s.jobs[j.ID]
	paused := stillRegistered && cur.Paused
	if stillRegistered && !paused {
		snap = snapshotJobLocked(j)
		snapTaken = true
	}
	s.mu.RUnlock()
	if !stillRegistered {
		slog.Debug("cron: job deleted during jitter window, aborting run",
			"job_id", j.ID, "run_id", runID)
		return jobSnapshot{}, false, true
	}
	if paused {
		slog.Debug("cron: job paused during jitter window, aborting run",
			"job_id", j.ID, "run_id", runID)
		return jobSnapshot{}, false, true
	}
	return snap, snapTaken, false
}

// resolveCronWorkspace resolves the snapshot's workDir into the path handed to
// the CLI wrapper, re-validating the allowedRoot containment at execute time.
// Extracted verbatim from executeOpt under R238-ARCH-2 (#734) so the run path
// reads as a sequence of named phases; behaviour is unchanged.
//
// Re-check allowedRoot at execute time to close the symlink-swap race:
// validateWorkspace at creation resolved symlinks once, but the target could
// have been retargeted since.
//
// R246-GO-12: when allowedRoot is set, hand the symlink-resolved path to the
// cli wrapper rather than the raw snap.workDir. The resolved path was just
// validated by EvalSymlinks; using it here makes the validation view match the
// open view and forecloses a final TOCTOU window between this check and the
// CLI's own open.
//
// R242-SEC-10 (#638): when allowedRoot is unset (sandbox disabled) the in-root
// containment short-circuit returns "" so we fall back to a best-effort
// EvalSymlinks (not bare filepath.Clean, which does NOT resolve symlinks). A
// workDir like /var/cron-jobs/foo could point through a symlink at an
// operator-unintended location, and the CLI would then chdir there with the
// only validation being "looks lexically clean". An EvalSymlinks failure
// (broken link, missing target, or insufficient perms to traverse) falls back
// to the cleaned raw input rather than aborting the run — losing resolution is
// preferable to refusing to run when sandbox is already off by operator choice.
//
// Returns abort=true (after emitting finishRun for the outside-root failure
// class) only on the allowedRoot-set, containment-rejected path; the caller
// MUST return immediately. All other paths return abort=false with the
// resolved path.
func (s *Scheduler) resolveCronWorkspace(
	j *Job, snap jobSnapshot, runID string, startedAt time.Time,
	trigger TriggerKind, lg *slog.Logger, finalizer *runFinalizer,
) (workDirForCLI string, abort bool) {
	if s.allowedRoot != "" {
		// R247-PERF-24: cached variant collapses repeated EvalSymlinks
		// for fast-firing jobs whose workDir / allowedRoot is stable.
		// TTL-bounded (workDirResolveCacheTTL) so a deliberate symlink
		// retarget surfaces within one notify-budget on the next tick.
		resolved, ok := s.workDirResolveUnderRootCached(snap.workDir)
		if !ok {
			lg.Warn("cron job work_dir outside allowed root; aborting run",
				"work_dir", snap.workDir)
			s.finishRun(finishArgs{
				job: j, runID: runID, startedAt: startedAt, trigger: trigger,
				state: RunStateFailed, errClass: ErrClassWorkDirOutsideRoot,
				errMsg: "work_dir outside allowed root",
				prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
				finalizer: finalizer,
			})
			return "", true
		}
		return resolved, false
	}
	if resolved, err := filepath.EvalSymlinks(snap.workDir); err == nil {
		return filepath.Clean(resolved), false
	}
	return filepath.Clean(snap.workDir), false
}

func (s *Scheduler) executeOpt(j *Job, viaTriggerNow bool) {
	// R20260526-GO-004: hot-path self-defence against a nil router. The
	// companion R20260526-GO-023 already logs at construction when
	// cfg.Router is nil, but tests build narrow fixtures via NewScheduler
	// without a router and only the fail-safe NPE-vs-skip distinction
	// matters at tick time. Without this guard the s.router.Reset call
	// inside freshContextPreflightP0 (line ~318) and s.router.GetOrCreate
	// (line ~712) NPE deep in the run loop, leaving CAS gates held and
	// triggerWG.Add already incremented. Short-circuit before any of that
	// state changes — the inflight CAS has not been taken yet, so an
	// early return is safe.
	if s == nil || s.router == nil {
		slog.Error("cron: router is nil; skipping run",
			"id", func() string {
				if j == nil {
					return ""
				}
				return j.ID
			}())
		// R20260527122801-CR-13 (#1323): emit a synthetic started→ended
		// pair so dashboard "running" counters and subscriber timelines
		// stay consistent. errClass=router_missing distinguishes this
		// degraded short-circuit from a real overlap_skipped. Guarded on
		// non-nil s + j: if s is nil there's no Scheduler to broadcast
		// from, and a nil j means we have no JobID to attach the frames
		// to either.
		if s != nil && j != nil {
			s.emitSyntheticSkipped(j, viaTriggerNow, ErrClassRouterMissing, "router unavailable", "router-missing")
		}
		return
	}
	// Guard against concurrent execution of the same job. The cron chain's
	// SkipIfStillRunning protects the scheduled-tick path, but TriggerNow
	// that arrives while a tick is in flight bypasses the chain entirely
	// (it calls execute directly when entryID == 0 or Run() on the entry
	// which is separately serialized). The per-job *runInflight (containing
	// the CAS atomic.Bool) keeps a uniform CAS gate while exposing run
	// metadata to the list API.
	inflight := s.jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		slog.Info("cron: job already running, skipping overlap", "job_id", j.ID)
		// Overlap is a skipped state (no LastRunAt update). Counters /
		// broadcast still fire so dashboards can surface the skip.
		s.emitOverlapSkipped(j, viaTriggerNow)
		return
	}
	// finalizer 是本次 run 的栈局部清理器。finishRun 在 emitRunEnded 之前
	// 调一次（让 broadcast 与 inflight view 同步可见 ok=false），下面的
	// defer 兜底覆盖 jitter-window 早返路径。done 标记由 finalize() 自身
	// 维护，run-A 的 defer 永远只看到 run-A 的 done=true，不会动到 run-B
	// 已抢占的 *runInflight 字段——并发隔离来自 finalizer 的 per-run 身份，
	// 不依赖 *runInflight 上的任何 atomic。R238-GO-2 + R246-GO-3 (#689).
	finalizer := &runFinalizer{inflight: inflight}
	defer func() {
		// R246-GO-3 (#689) 取代了 R246-CR-017 (#759) 把 reset + CAS-release
		// 抽到 inflight.releaseRun 的设计：那个共享 *runInflight 上的方法
		// 无法防 run-A 的迟到 defer clobber run-B 已抢占的字段。改用
		// per-run 栈局部 finalizer：done 标志保证 run-A 的 defer 只看到
		// 自己的 finalizer，绝不会动到 run-B 的元数据。reset → CAS-release
		// 的内部顺序（R238-GO-2）在 finalize() 内部保留。Gauge 的 -1 留在
		// 这里与 executeOpt 入口的 +1 视觉配对。
		finalizer.finalize()
		metrics.CronRunInflight.Add(-1)
	}()
	// R242-CR-14 (#706): metrics.CronRunInflight semantically tracks "how
	// many jobs hold the CAS slot right now", which is exactly the window
	// the defer guards. The historical placement was after metadata
	// population — fine when the only exit between defer and Add(+1) was
	// success, but the new generateRunID error branch returns before
	// reaching it, so the defer's Add(-1) would underflow the gauge.
	// Hoisting Add(+1) here pairs it with the defer's Add(-1) on every
	// early-return path (rand failure today, future preconditions later).
	metrics.CronRunInflight.Add(1)

	// R20260527122801-CR-8 (#1322): post-CAS paused/deleted recheck. The
	// callers (executeJobIDIfLive for TriggerNow and the registerJob
	// AddFunc closure for scheduled ticks) check paused/deleted under
	// s.mu.RLock and release the lock BEFORE invoking executeOpt. There is
	// a narrow 1-2µs window between that release and the CAS above where a
	// concurrent PauseJobByID / DeleteJobByID can land — the original
	// jitter-window recheck (line ~902-915 below) only fires in the
	// `!viaTriggerNow && s.jitterMax > 0` branch, leaving TriggerNow and
	// the jitter==0 scheduled path unprotected. Recheck once here, after
	// CAS but before any heavy work (snapshot / spawn / send), so a Pause
	// that lands in the cross-lock window aborts the run cleanly. The
	// post-CAS placement also subsumes the existing jitter-window recheck
	// for the in-jitter case — Pause that lands DURING jitter is still
	// caught by that block's recheck after the sleep returns. The defer
	// above handles inflight CAS release + gauge decrement on this early
	// return.
	{
		s.mu.RLock()
		curCAS, stillRegisteredCAS := s.jobs[j.ID]
		pausedCAS := stillRegisteredCAS && curCAS.Paused
		s.mu.RUnlock()
		if !stillRegisteredCAS || pausedCAS {
			// R243-ARCH-13 (#841): both cross-lock abort branches log the
			// same {job_id, trigger_now} pair; bind it once via slog.With.
			casLg := slog.With("job_id", j.ID, "trigger_now", viaTriggerNow)
			if !stillRegisteredCAS {
				casLg.Debug("cron: job deleted between dispatch lookup and CAS, aborting run")
				// R040034-CR-1 (#1410): emit synthetic started→ended pair so
				// dashboard subscribers see a complete lifecycle frame instead
				// of a 1-2µs gap when Delete lands in the cross-lock window.
				// Mirrors the router-missing precedent at the top of
				// executeOpt and the overlap-skipped emit on CAS-lost.
				s.emitSyntheticSkipped(j, viaTriggerNow, ErrClassDeletedConcurrent, "job deleted between dispatch and CAS", "deleted-during-dispatch")
			} else {
				casLg.Debug("cron: job paused between dispatch lookup and CAS, aborting run")
				// R040034-CR-1 (#1410): see DeletedConcurrent above. Pause
				// landing in the cross-lock window also gets a synthetic pair
				// so the dashboard "running" counter stays consistent.
				s.emitSyntheticSkipped(j, viaTriggerNow, ErrClassPausedConcurrent, "job paused between dispatch and CAS", "paused-during-dispatch")
			}
			return
		}
	}

	// Populate the inflight metadata under the CAS-true window. RunID is
	// generated once per run; StartedAt is captured before jitter so the
	// "running 12s" badge in the UI counts true wall-clock from CAS.
	runID, err := generateRunID()
	if err != nil {
		// R242-CR-14 (#706): crypto/rand 不可用时不能 panic 整个进程 ——
		// cron tick 是后台 goroutine，panic 会被 robfig/cron 的 wrapper
		// recover 但接下来这个 job 的 entry 也不会再正常工作；不如直接
		// log + skip 该次 tick，下一周期自然恢复（getrandom 失效是瞬时的
		// 内核事件）。defer 已经覆盖 inflight 释放 + CronRunInflight
		// 配对，无需手动清理。
		slog.Error("cron: failed to generate run ID; skipping tick",
			"job_id", j.ID, "trigger_now", viaTriggerNow, "err", err)
		return
	}
	startedAt := time.Now()
	trigger := TriggerScheduled
	if viaTriggerNow {
		trigger = TriggerManual
	}
	// R238-ARCH-3 (#742): single atomic.Pointer.Store with the complete
	// view replaces the prior 6 separate atomic.Pointer Stores. Readers
	// that snapshot() during this populate observe either the prior view
	// (still ok semantically — running gate already guards them) or the
	// complete new view; never a half-populated mix.
	inflight.populate(runInflightView{
		RunID:     runID,
		StartedAt: startedAt,
		Phase:     PhaseQueued,
		Trigger:   trigger,
	})
	// R247-GO-1: freshSnap is set authoritatively from snap.fresh after
	// snapshotJob runs under s.mu (line ~447); writing j.FreshContext here
	// without the lock was redundant and -race-suspect.
	// CronRunStartedTotal bumps inside emitRunStarted (R230C-GO-15).

	// snap is populated either inside the jitter block (folded RLock with
	// the post-jitter recheck — R20260528-PERF-2 / #1351) or by the
	// fall-through snapshotJob() call below. snapTaken tracks which path
	// won so we never double-snapshot — taking it twice would risk the
	// second read seeing a fresher UpdateJob than the recheck observed,
	// silently violating the "post-jitter snapshot reflects the same
	// instant the recheck verified" contract.
	var snap jobSnapshot
	var snapTaken bool

	// Apply jitter after CAS, before snapshot. After-CAS so concurrent overlap
	// triggers are rejected immediately. Before-snapshot so an UpdateJob that
	// lands during the jitter window still lets the subsequent snapshot read
	// the new Prompt / WorkDir (matches the "edits take effect immediately"
	// operator expectation). TriggerNow skips jitter to preserve the
	// "run now = run now" semantics.
	if !viaTriggerNow && s.jitterMax > 0 {
		var abort bool
		snap, snapTaken, abort = s.applyJitterAndRecheck(j, runID, inflight)
		if abort {
			return
		}
	}

	// Snapshot mutable Job fields once under s.mu so the rest of the
	// execution can run lock-free; concurrent SetJobPrompt/UpdateJob land
	// for the next tick rather than racing this in-flight result. The
	// jitter-enabled path already populated snap inside the recheck's
	// RLock window — skip the redundant call here.
	if !snapTaken {
		snap = s.snapshotJob(j)
	}
	inflight.setFresh(snap.fresh)

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
	//
	// R238-PERF-2 / R245-PERF-7 / R242-PERF-3 (#849, #858, #666): one
	// slog.With per execution allocates a 4-attr Logger handler chain. We
	// deliberately keep this pattern despite the alloc: (a) the chain is
	// reused 20+ times below (success Info + send-deadline Warn +
	// session-error Error + the finishRun routing fan-out), so amortised
	// cost per use is sub-µs; (b) Caching on *Job would require
	// invalidation on every SetJobPlatform / SetJobChatID mutation — a
	// correctness liability disproportionate to ~200 ns saved per cron
	// tick; (c) Caching on *runInflight or jobSnapshot is per-execution
	// scope, identical to the local `lg` and only adds an indirection.
	// Lazy build via sync.Once would not help because line below
	// unconditionally triggers the alloc on first .Info call. The
	// cron-tick path's hot allocs are dominated by snapshot copy + CLI
	// subprocess spawn — optimising the logger here would not move the
	// needle. Leave the alloc; document the rationale so future reviewers
	// don't reopen it. #666 is the latest [REPEAT-N] — closing as
	// won't-fix-by-design.
	lg := slog.With("job_id", snap.jobID, "platform", snap.platName, "chat", snap.chatID, "run_id", runID)
	lg.Info("cron job executing", "prompt_len", len(snap.prompt))

	// Per-job timeout is always s.execTimeout (period scaling was removed —
	// robfig/cron's SkipIfStillRunning chain wrapper drops a colliding tick
	// instead of killing a long-running job, so the deadline does not need
	// to anticipate the next tick).
	jobTimeout := s.execTimeout
	// spawnCtx 是 GetOrCreate 阶段的超时上下文，从 GetOrCreate 返回后到
	// finishRun 之间这条 ctx 不再有任何消费者；让其底层 timer 一直挂到
	// executeOpt return 才被 defer 释放，意味着 N 个并发 in-flight job
	// (上限 maxJobsHardCap=500) 会在整个 Send 阶段 (≤jobTimeout) 占着
	// 等同的 *time.Timer 槽位。R250-GO-15 (#1078): 显式在 GetOrCreate
	// 出口 cancel()；defer 仍兜底（cancel 幂等，二次调用 no-op），早 free
	// 掉这条 timer 后续 ≤jobTimeout 都不再压 runtime timerproc。
	ctx, spawnCancel := context.WithTimeout(s.stopCtx, jobTimeout)
	defer spawnCancel()

	// agentCommands and agents are published once at scheduler construction
	// (cfg.AgentCommands / cfg.Agents) via configMapsPtr and never swapped
	// today; reading them lock-free through configMaps() is safe. A future
	// SetAgents/hot-reload API Store()s a fresh *cronConfigMaps so this read
	// stays race-free without moving under s.mu (R249-ARCH-27 / #991). Load
	// the snapshot once so both reads see the same generation.
	cm := s.configMaps()
	agentID, cleanText := resolveAgent(snap.prompt, cm.agentCommands)
	opts := cloneAgentOpts(cm.agents[agentID])
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
		workDirForCLI, abort := s.resolveCronWorkspace(j, snap, runID, startedAt, trigger, lg, finalizer)
		if abort {
			return
		}
		opts.Workspace = workDirForCLI
	}
	key := sessionkey.CronKey(snap.jobID)

	// Fresh mode: drop any existing session (and its process + history) so
	// GetOrCreate spawns a brand-new CLI. The helper handles ctx-cancel,
	// workDir reachability, and post-Reset job-existence re-check. On
	// error paths the returned stubRefresh re-registers the sidebar row
	// so the cron entry doesn't vanish from the dashboard. On the success
	// path we skip stubRefresh because the live session carries its own
	// sidebar entry. Persistent mode short-circuits inside the helper
	// with a no-op stubRefresh.
	stubRefresh, ok := s.freshContextPreflightP0(preflightArgs{
		job: j, snap: snap, key: key, lg: lg, notifyTo: notifyTo,
		runID: runID, startedAt: startedAt, trigger: trigger,
		finalizer: finalizer,
	})
	if !ok {
		stubRefresh.run()
		return
	}

	inflight.setPhase(PhaseSpawning)
	// R250-CR-22 (#1155): capture spawnStart immediately before GetOrCreate
	// so the "send budget exceeds job/2" warn at line ~831 measures actual
	// spawn time, not (jitter + spawn). startedAt is captured pre-jitter for
	// the dashboard "running 12s" badge (true wall-clock from CAS), but the
	// warn is calibrated against jobTimeout/2 to detect when the spawn phase
	// alone consumed too much of the budget — folding jitter (default up to
	// 30s) into that measurement triggers false positives on healthy jobs
	// whose schedule landed unlucky in the jitter window.
	spawnStart := time.Now()
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
				finalizer: finalizer,
			})
			stubRefresh.run()
			return
		}
		state, errClass := classifyExecError(err, ErrClassSessionError)
		if errClass == ErrClassDeadlineExceeded {
			lg.Info("cron session deadline exceeded", "err", err)
		} else {
			// R20260603-SEC-1: sanitise before logging to strip IP:port / paths.
			lg.Error("cron session error", "err", sanitiseRunErrMsg(err.Error()))
		}
		s.finishRun(finishArgs{
			job: j, runID: runID, startedAt: startedAt, trigger: trigger,
			state: state, errClass: errClass, errMsg: "session error: " + err.Error(),
			prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
			finalizer: finalizer,
		})
		s.deliverNotice(notifyTo, formatCronNotice(snap.labelOrID(), "执行跳过，请稍后重试。"))
		stubRefresh.run()
		return
	}
	// R250-GO-15 (#1078): GetOrCreate consumed spawnCtx; nothing below references
	// it (Send uses sendCtx). Cancel now to free the underlying *time.Timer
	// instead of waiting for the function-scoped defer at executeOpt return.
	// On a 500-job-deep deployment with 5min jobTimeout this trims up to
	// ~500 idle timers off runtime timerproc during the Send window. The
	// outer defer remains as a safety net (cancel is idempotent: second
	// invocation is a no-op). Must come AFTER the err-return block above
	// so an early return on session error still trips the defer (already
	// covered) — placing here keeps the explicit cancel on the success
	// path only, mirroring the issue's "after the err handling" guidance.
	spawnCancel()

	// R242-ARCH-22 (#766): populate inflight.SessionID as soon as
	// GetOrCreate returns. Persistent-mode runs reuse a session that
	// already carries its CLI session_id (set during the original spawn's
	// init handshake), so sess.SessionID() is non-empty here. Fresh-mode
	// runs spawn a new CLI whose session_id is only stamped after the
	// init turn completes — sess.SessionID() returns "" in that window
	// and the post-Send setSessionID below remains the authoritative
	// write. Without this early capture, KnownSessionIDs / IsExcluded
	// probes during the Send window miss the in-flight run on
	// persistent-mode jobs (the auto-workspace-chain feature then
	// momentarily considers the cron session a candidate for prev_session_ids
	// until Send completes). setSessionID is idempotent and same-value
	// writes fast-path so the post-Send call is a no-op when the IDs match.
	if sid := sess.SessionID(); sid != "" {
		inflight.setSessionID(sid)
	}

	// R238-GO-4 / R236-GO-07 (#790, #500): Send is parented on s.stopCtx
	// so Scheduler.Stop() can short-circuit an in-flight cron Send instead
	// of letting it run for up to jobTimeout (default 5min) after Stop
	// returns — the historical Background parent created a use-after-free
	// class race where Send could write to a session that Router.Shutdown
	// had already reclaimed. The errors.Is(err, context.Canceled) branch
	// below already handles the cancel case with skipPersist=true, so a
	// Stop()-canceled Send no longer logs as a failure, no LastRunAt is
	// stamped, and the job re-runs on the next Start (matching the spawn
	// path's GetOrCreate cancel handling immediately above).
	//
	// R20260527122801-CR-2 (#1311): clamp sendCtx to the remaining budget
	// after spawn consumed `time.Since(spawnStart)` of jobTimeout, with a
	// minSendBudget floor to preserve the historical concern (R230B-GO-1 /
	// R222-GO-1) that flaky cold-start spawns shouldn't immediately surface
	// as "send timed out" — a 30s floor still lets a healthy Send complete
	// while bounding worst-case wall-clock to (jobTimeout + minSendBudget)
	// instead of ~2*jobTimeout. Operators previously saw systemd
	// TimeoutStopSec exceeded on cron runs because the un-clamped sendCtx
	// could double the per-run budget; the floor + spawnElapsedWarnRatio
	// warn (just below) keep the operator-signal path intact.
	sendBudget := jobTimeout - time.Since(spawnStart)
	if sendBudget < minSendBudget {
		sendBudget = minSendBudget
	}
	sendCtx, sendCancel := context.WithTimeout(s.stopCtx, sendBudget)
	defer sendCancel()
	// R240-GO-4: emit an explicit signal when entering sendCtx after the
	// spawn phase already consumed >spawnElapsedWarnRatio of jobTimeout.
	// The wall-clock doubling described above is intentional but
	// historically silent; operators of 300s+ jobs need a structured
	// event to drive runbook alerts. Counter + slog pair (mirrors
	// CronExecutionSlowTotal + "cron execution slow" lower in this same
	// function). R247-CR-28: ratio extracted to a documented const so
	// future tuning is a one-line change with shared rationale.
	spawnWarnBudget := time.Duration(float64(jobTimeout) * spawnElapsedWarnRatio)
	// R250-CR-22 (#1155): time.Since(spawnStart) — not startedAt — so jitter
	// time is excluded. See spawnStart capture above (just before GetOrCreate).
	if spawnElapsed := time.Since(spawnStart); spawnElapsed > spawnWarnBudget {
		metrics.CronSendBudgetDoubledTotal.Add(1)
		// Message string preserved for runbook grep — see docs/ops/pprof.md
		// + internal/metrics/metrics.go CronSendBudgetDoubledTotal godoc.
		lg.Warn("cron send budget exceeds job/2",
			"job_id", snap.jobID,
			"spawn_elapsed_ms", spawnElapsed.Milliseconds(),
			"job_timeout_ms", jobTimeout.Milliseconds(),
			// R20260527122801-CR-2 (#1311): send_budget now reflects the
			// post-clamp budget (jobTimeout - spawnElapsed, floored at
			// minSendBudget) instead of the historical un-clamped jobTimeout.
			// Operators reading this warn line can compare spawn_elapsed +
			// send_budget against jobTimeout to see the floor in action.
			"send_budget_ms", sendBudget.Milliseconds(),
			"warn_ratio", spawnElapsedWarnRatio)
	}
	inflight.setPhase(PhaseSending)

	// R215-ARCH-P2-5 (#581) partial: the Send + watchdog + abort-drain
	// trio is a self-contained sub-machine that doesn't need to share
	// stack frame with the surrounding executeOpt. Extracting it
	// localises the watchdog ↔ Send ordering contract (drain abortCh
	// AFTER cancelling sendCtx) so a future executeOpt split doesn't
	// accidentally reorder the lines and let the next Reset race the
	// in-flight interrupt write. See sendWithWatchdog godoc for the
	// invariant.
	result, abort, err := sendWithWatchdog(sendCtx, sendCancel, sess, cleanText)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Same rationale as the session-error branch above: suppress
			// the operator-facing notice so shutdown races don't look like
			// real failures. abort.fired can still be true here when a
			// stopCtx cancel races a near-deadline tick — surface it so
			// operators have a signal that an interrupt attempt happened
			// during the cancel path.
			//
			// R242-GO-7 (#555): mirror the DeadlineExceeded branch below —
			// when the watchdog fired but the interrupt did not land
			// (outcome neither InterruptSent nor InterruptUnsupported), the
			// in-flight turn may still be wedged at session level even
			// though the cron run is recorded as cancelled. Operators need
			// the Warn signal to investigate transport-level breakage in
			// the same shape as the deadline path; otherwise a "fired-but-
			// silent" interrupt during a cancel-deadline race is buried at
			// Info severity and slips past log alerts.
			if abort.fired && abort.outcome != InterruptSent &&
				abort.outcome != InterruptUnsupported {
				lg.Warn("cron send cancelled; interrupt did not land",
					"err", err,
					"abort_fired", abort.fired,
					"abort_outcome", abort.outcome)
			} else {
				lg.Info("cron send cancelled",
					"err", err,
					"abort_fired", abort.fired,
					"abort_outcome", abort.outcome)
			}
			s.finishRun(finishArgs{
				job: j, runID: runID, startedAt: startedAt, trigger: trigger,
				state: RunStateCanceled, errClass: ErrClassCanceled, errMsg: err.Error(),
				skipPersist: true,
				prompt:      snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
				finalizer: finalizer,
			})
			stubRefresh.run()
			return
		}
		state, errClass := classifyExecError(err, ErrClassSendError)
		if errClass == ErrClassDeadlineExceeded {
			// Log alongside the watchdog outcome so operators see both the
			// deadline AND whether the CLI was successfully interrupted in
			// the same line. ACP backends report "unsupported" here — we
			// accept the silent no-op since ACP cron jobs are rare and a
			// SIGINT fallback would couple two different abort semantics.
			//
			// R242-GO-7: when the watchdog fired but the interrupt did not
			// reach the CLI (outcome != InterruptSent and != InterruptUnsupported
			// for ACP), surface as Warn — the in-flight turn may still be
			// burning Send budget on the next tick, and operators need a
			// signal to investigate transport-level breakage. The
			// InterruptUnsupported tag is excluded by design: ACP jobs
			// always report unsupported and would otherwise spam Warn.
			if abort.fired && abort.outcome != InterruptSent &&
				abort.outcome != InterruptUnsupported {
				lg.Warn("cron send deadline exceeded; interrupt did not land",
					"err", err,
					"abort_fired", abort.fired,
					"abort_outcome", abort.outcome)
			} else {
				lg.Info("cron send deadline exceeded",
					"err", err,
					"abort_fired", abort.fired,
					"abort_outcome", abort.outcome)
			}
		} else {
			// R20260603-SEC-4: sanitise before logging to strip IP:port / paths.
			lg.Error("cron send error", "err", sanitiseRunErrMsg(err.Error()))
		}
		s.finishRun(finishArgs{
			job: j, runID: runID, startedAt: startedAt, trigger: trigger,
			state: state, errClass: errClass, errMsg: "send error: " + err.Error(),
			prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
			finalizer: finalizer,
		})
		s.deliverNotice(notifyTo, formatCronNotice(snap.labelOrID(), "执行失败，请稍后重试。"))
		stubRefresh.run()
		return
	}
	if result.SessionID != "" {
		inflight.setSessionID(result.SessionID)
	}

	elapsed := time.Since(startedAt)
	lg.Info("cron job completed",
		"result_len", len(result.Text),
		"elapsed_ms", elapsed.Milliseconds())
	// OBS1 (#392): record the full success-path latency distribution, not
	// just the slow-tail count below. The histogram buckets straddle
	// slowThreshold so the two signals stay consistent (anything past 30s
	// lands in the same tail buckets the slow counter alerts on). Observed
	// here rather than in finishRun for the same reason as the slow counter:
	// only success-path latency is meaningful — error/timeout paths are
	// classified by the CronRun*Total state counters instead.
	metrics.ObserveCronExecutionDuration(elapsed.Milliseconds())
	slowThreshold := s.slowThreshold
	if slowThreshold <= 0 {
		slowThreshold = defaultCronSlowThreshold
	}
	if elapsed > slowThreshold {
		// R208-OBS1: poor-man's histogram — a single counter that fires
		// when a successful execution takes longer than slowThreshold.
		// Wired here (not in finishRun) so only success-path latency
		// counts; error paths already surface via metrics state counters.
		// R241-ARCH-11 (#519): threshold reads s.slowThreshold (config-
		// supplied) with the package default as fallback.
		metrics.CronExecutionSlowTotal.Add(1)
		lg.Warn("cron execution slow",
			"job_id", snap.jobID,
			"elapsed_ms", elapsed.Milliseconds(),
			"threshold_ms", slowThreshold.Milliseconds())
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
		finalizer: finalizer,
	})

	// R234-SEC-1: deliverNotice 必须用经过 sanitiseRunResult 的文本，
	// 否则未截断 / 未脱敏的 claude 输出会绕过所有保护落到 IM 渠道
	// （prompt-injection / IM 富文本指令 / 巨量响应耗尽队列）。
	// finishRun 在持久化路径已做过同样处理，这里复用相同管线。
	//
	// R20260531070014-ARCH-1: claude -p 可 exit 0 但 result.Text 为
	// API-error envelope（含 request ID / 内部 hostname / 泄漏 cred）。
	// dispatch IM 路径已通过 localizeAPIError（封装 apierr.Localize）防护；
	// cron 成功路径之前完全绕过此保护。先 sanitise（截断/脱敏）再 localize
	// （本地化/隐藏敏感 envelope），顺序以隐私优先。
	replyText := formatCronNotice(snap.labelOrID(), apierr.Localize(sanitiseRunResult(result.Text)))
	s.deliverNotice(notifyTo, replyText)
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
//
// R246-GO-22: NewTimer/defer Stop 在每次 tick 都分配 *time.Timer，
// 当前规模（~100 timer/min @ 100 jobs * 1Hz）成本可忽略，无需优化。
// 未来若 job 数突破 ~5000/min（≈ 80 alloc/s）再考虑 sync.Pool[*time.Timer]
// 或退化到 runtime.timeSleep 直接路径；提前优化只会让控制流更晦涩。
// time.After(d) 同样会 alloc *Timer 但不能被 Stop()，ctx 取消时会泄漏到
// 触发点为止，不适合此处。
func applyJitter(ctx context.Context, schedule string, jitterMax time.Duration) {
	if jitterMax <= 0 {
		return
	}
	// R250-CR-14 (#1147): the string-keyed entry point re-parses on every
	// call. Production now prefers applyJitterSched with the pre-parsed
	// robfigcron.Schedule pulled from s.cron.Entry, but this signature is
	// retained for tests and for the fallback path when entryID is 0 /
	// concurrently removed. Keep the parse → period → sleep pipeline
	// behaviourally identical to applyJitterSched so the two paths cannot
	// diverge.
	period := schedulePeriod(schedule, time.Now())
	jitterSleep(ctx, period, jitterMax)
}

// applyJitterSched is the entry point for the cron tick hot path. It reuses
// the already-parsed robfigcron.Schedule that the cron engine holds inside
// each Entry, avoiding a redundant cronParser.Parse on every tick. Behaviour
// is otherwise identical to applyJitter — same window cap (period/4), same
// jitterMax fallback, same ctx.Done() short-circuit. R250-CR-14 / #1147.
func applyJitterSched(ctx context.Context, sched robfigcron.Schedule, jitterMax time.Duration) {
	if jitterMax <= 0 {
		return
	}
	var period time.Duration
	if sched != nil {
		period = schedulePeriodFromSched(sched, time.Now())
	}
	jitterSleep(ctx, period, jitterMax)
}

// jitterSleep is the shared tail of applyJitter / applyJitterSched: clamp
// jitterMax by period/4 (with period<=0 meaning "use jitterMax as-is"),
// roll a random duration in [0, window), and sleep on a Timer that respects
// ctx cancellation. Extracted so the parse-once vs reuse-parsed split lives
// only in the two thin entry points above. R250-CR-14 / #1147.
func jitterSleep(ctx context.Context, period, jitterMax time.Duration) {
	window := jitterMax
	if period > 0 {
		if quarter := period / 4; quarter < window {
			window = quarter
		}
	}
	if window <= 0 {
		return
	}
	// R20260527122801-GO-018 defensive: int64(window) underflow guard
	// so future Schedule providers returning non-monotonic Next don't
	// panic Int64N. window is already a time.Duration (int64) and the
	// `window <= 0` check above covers the normal range, but a hostile
	// or buggy custom Schedule could conceivably produce a period that
	// arithmetic clamps to a non-positive int64; mrand.Int64N panics
	// on n <= 0, so a single extra branch keeps the tick goroutine
	// from going down to robfig/cron's recover path.
	n := int64(window)
	if n <= 0 {
		return
	}
	d := time.Duration(mrand.Int64N(n))
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

// cloneAgentOpts returns a shallow copy of opts with all reference-typed
// fields (slices / maps) defensively cloned so downstream `append` /
// in-place writes cannot mutate the entry stored in Scheduler.agents.
//
// R246-GO-17 / R228-GO-P3-8: previous code only clipped ExtraArgs.
// Today AgentOpts only carries one slice field (ExtraArgs) — plus
// strings/bool — so clipping was sufficient. This helper centralises the
// clone so any future field added to cron.AgentOpts (e.g. an Env map
// or HookConfigs slice) gets defensive copy automatically rather than
// leaking shared state into the per-run mutated copy. Keep this pure /
// allocation-light: it sits on the cron run hot path.
func cloneAgentOpts(opts AgentOpts) AgentOpts {
	if len(opts.ExtraArgs) > 0 {
		// Slice-clone (full copy) rather than three-index clip because the
		// caller may overwrite individual indices, not just append. Cost
		// dominated by the typical 0–3 args; negligible vs spawn syscalls.
		out := make([]string, len(opts.ExtraArgs))
		copy(out, opts.ExtraArgs)
		opts.ExtraArgs = out
	}
	return opts
}
