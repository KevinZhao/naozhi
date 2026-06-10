// scheduler_notice.go: cron IM-notice formatting + the job snapshot that
// feeds notice labels.
//
// Split out of scheduler_run.go (move-only, no behaviour change): the
// notice-prefix const trio + formatCronNotice + escapeCronMarkdownPunct +
// jobSnapshot (and its labelOrID / snapshotJob / snapshotJobLocked) are the
// data + formatter that the run path funnels every deliverNotice through.
// None read s.stopCtx; methods stay on *Scheduler / jobSnapshot so private
// fields remain accessible without exporting.

package cron

import (
	"strings"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
)

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
	// placement 是 snapshot 时刻的 Job.Placement（""≡local）。executeOpt
	// 据此在 router 路径前分流到 sandbox 执行器（RFC §4.2 placement 轴）。
	placement string
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
// `[`, `]`, `(`, `)` with full-width visually-similar codepoints so an
// attacker-controlled cron Title or result body cannot smuggle `[text](url)`
// clickable links into the IM notice. R260528-SEC-8.
//
// R20260603140013-ARCH-1 (#1707): the escaping logic + its package-level
// Replacer moved to the leaf package internal/textutil so the IM dispatch
// edge can use it without importing this domain package. Thin alias kept so
// internal cron call sites (formatCronNotice) stay unchanged.
func escapeCronMarkdownPunct(s string) string {
	return textutil.EscapeCronMarkdownPunct(s)
}

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
		placement:     j.Placement,
		lastSessionID: j.LastSessionID,
	}
	// R090135-PERF-003 (#1931): alias j.Notify directly instead of deep-
	// copying via `v := *j.Notify; snap.notify = &v`. The old form forced
	// `v` to the heap (the returned snapshot escapes the function), costing
	// one *bool alloc per snapshot — 50 jobs × 1Hz = 50 alloc/s under
	// executeOpt. The deep copy was never needed for correctness: UpdateJob
	// (scheduler_jobs.go) only ever *reassigns* j.Notify to a fresh &v (or
	// nil) under s.mu.Lock — it never mutates *j.Notify in place — so the
	// pointed-to bool is immutable once published. Reading the pointer slot
	// under s.mu (the snapshot's RLock) yields a stable pointer to an
	// immutable value; the sole downstream reader (resolveNotifyDecision)
	// only nil-checks and derefs, never writes through it. Aliasing is
	// therefore alloc-free and tear-free.
	snap.notify = j.Notify
	return snap
}
