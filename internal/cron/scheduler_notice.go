// scheduler_notice.go: cron IM-notice formatting (notice-prefix consts +
// formatCronNotice + escapeCronMarkdownPunct + failureNoticeBody) and the
// jobSnapshot that feeds notice labels. None read s.stopCtx; methods stay on *Scheduler / jobSnapshot
// so private fields remain accessible without exporting.

package cron

import (
	"strconv"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
)

// jobSnapshot captures the mutable Job fields executeOpt reads under s.tbl.mu so
// the long-running send/notify pipeline can run without holding the lock.
// Snapshot is taken once after the rate-limit/jitter gate and reused for the
// rest of the execution; concurrent SetJobPrompt/UpdateJob therefore land
// for the next tick rather than racing the in-flight result.
//
// 字段按 size DESC 排，消除 string/bool/*bool 混排引入的 padding。
type jobSnapshot struct {
	prompt  string
	workDir string
	jobID   string
	// label is the human-readable title for IM notice prefixes, computed via
	// jobTitleOrFallback under s.tbl.mu so a concurrent SetJobPrompt cannot tear
	// Title vs Prompt-derived fallback. Empty when both are blank — labelOrID
	// then falls back to jobID so the prefix never collapses to "[Cron ]".
	label      string
	platName   string
	chatID     string
	notifyPlat string
	notifyChat string
	schedule   string
	backend    string // "" = router default
	// lastSessionID 是 snapshot 时刻 Job.LastSessionID 的拷贝，供 fresh-preflight
	// 的 stub-refresh 闭包直接调 registerStubByValue，不再回头加 s.tbl.mu 读。失败
	// 路径在没有 result 帧时用 snap-time chain anchor；后续产生 result 帧的 run
	// （成功或 turn_failed）由 finishRun 再覆写。
	lastSessionID string
	notify        *bool // nil = unset
	fresh         bool
	// placement 是 snapshot 时刻的 Job.Placement（""≡local）。executeOpt
	// 据此在 router 路径前分流到 sandbox 执行器（RFC §4.2 placement 轴）。
	placement string
	// sideEffects 是 snapshot 时刻的 Job.SideEffects（nil→false）。sandbox
	// failed-transport 路径据此决定是否进人工确认队列（§6.2 双跑围栏）。
	sideEffects bool
}

// cronNoticePrefixFmt is the IM-notice prefix template every cron-side
// deliverNotice call funnels through; new notice sites should compose via
// formatCronNotice rather than inline a copy. The formatter inlines the shape
// ("[Cron <label>] <body>") via strings.Builder, so a template change must
// touch both this literal and the segment consts below —
// notice_label_bracket_test pins the byte sequence.
const cronNoticePrefixFmt = "[Cron %s] %s"

// cronNoticePrefix / cronNoticeMid are the literal segments stitched into
// formatCronNotice's strings.Builder output; kept as separate consts so a
// template change cannot silently desync from cronNoticePrefixFmt.
const (
	cronNoticePrefix = "[Cron "
	cronNoticeMid    = "] "
)

// formatCronNotice renders the IM-notice line cron jobs send through
// deliverNotice. label is snap.labelOrID(); body is the human-readable suffix
// already in the caller's display locale. Pure formatter so it can be reused
// outside the execute path.
//
// SECURITY: label reaches the IM channel without transiting sanitiseRunResult,
// so an attacker-supplied job Title (e.g. "‮…" RLO) — which AddJob's
// MaxCronTitleLen check does not strip — would reverse the surrounding text.
// It is forced through osutil.SanitizeForLog (C0/C1, bidi overrides +
// isolates, LS/PS); applying it to body as well is idempotent defence-in-depth.
func formatCronNotice(label, body string) string {
	// MaxCronTitleLen (256 runes) bounds label after the rune-count gate at
	// AddJob/UpdateJob — a 4× rune→byte budget lets CJK / emoji round-trip
	// through SanitizeForLog without truncation.
	label = osutil.SanitizeForLog(label, MaxCronTitleLen*4)
	// Replace markdown link-syntax `[` `]` `(` `)` with full-width look-alikes
	// so an attacker-controlled Title or result body cannot smuggle
	// `[text](url)` clickable links into IM notices (#1095). validateCronTitle
	// blocks bidi / C0 but lets ASCII punctuation through, so this is the
	// safety bottom line.
	label = escapeCronMarkdownPunct(label)
	body = escapeCronMarkdownPunct(body)
	// strings.Builder instead of fmt.Sprintf (#539); pre-grow once so the
	// buffer covers the largest plausible payload.
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
// clickable links into the IM notice. Thin alias over the leaf-package
// implementation in internal/textutil, shared with IM dispatch (#1707).
func escapeCronMarkdownPunct(s string) string {
	return textutil.EscapeCronMarkdownPunct(s)
}

// failureNoticeBody is the IM body for a run that did not succeed: the cause
// named from errClass/state, then the run id's first 8 hex chars (the same
// short form the dashboard history shows). timeout is the run's wall-clock
// budget, printed on the timed-out bodies. Never carries the raw error text.
func failureNoticeBody(errClass ErrorClass, state RunState, runID string, timeout time.Duration) string {
	timedOut := state == RunStateTimedOut || errClass == ErrClassDeadlineExceeded
	var cause string
	switch {
	case errClass == ErrClassSandboxTransport && timedOut:
		cause = "云沙箱运行超时（超过 " + formatNoticeBudget(timeout) + "），任务状态未知，请检查执行历史"
	case errClass == ErrClassSandboxTransport:
		cause = "云沙箱连接中断，任务状态未知，请检查执行历史"
	case timedOut:
		cause = "执行超时（超过 " + formatNoticeBudget(timeout) + "）"
	case errClass == ErrClassSessionCapacity:
		cause = "定时任务会话数已达上限，本次已跳过；可错开执行时间或改为每次重置上下文"
	case errClass == ErrClassSessionError:
		cause = "启动会话失败"
	case errClass == ErrClassSendError:
		cause = "执行失败（CLI 发送错误）"
	case errClass == ErrClassTurnFailed:
		cause = "执行失败（后端报告本轮出错），请检查执行历史"
	case errClass == ErrClassWorkDirUnreachable:
		cause = "工作目录不可达，本次执行已跳过"
	case errClass == ErrClassWorkDirOutsideRoot:
		cause = "工作目录超出允许根目录，本次执行已跳过"
	case errClass == ErrClassSandboxFailed:
		cause = "云沙箱任务失败"
	case errClass == ErrClassSandboxUnavailable:
		cause = "云沙箱未配置，任务无法执行"
	default:
		cause = "执行失败"
	}
	if len(runID) > 8 {
		runID = runID[:8]
	}
	if runID == "" {
		return cause
	}
	return cause + " · run " + runID
}

// autoPauseNoticeSuffix is appended to a failure notice when that run's
// failure auto-paused the job; pausedAfter is finishRun's result (0 = not
// paused). It says where to resume: an IM job by /cron resume in its chat,
// a dashboard job from the dashboard.
func autoPauseNoticeSuffix(snap jobSnapshot, pausedAfter int) string {
	if pausedAfter <= 0 {
		return ""
	}
	how := "修复后发送 /cron resume " + snap.jobID + " 恢复"
	if snap.platName == "dashboard" {
		how = "修复后在控制台恢复"
	}
	return "；已连续失败 " + strconv.Itoa(pausedAfter) + " 次，任务已自动暂停，" + how
}

// deliverFailureNotice sends the IM notice for a run of rc that did not
// succeed: failureNoticeBody plus, when pausedAfter > 0, the auto-pause
// sentence.
func (s *Scheduler) deliverFailureNotice(rc runCtx, errClass ErrorClass, state RunState, timeout time.Duration, pausedAfter int) {
	s.deliverNotice(rc.notifyTo, formatCronNotice(rc.snap.labelOrID(),
		failureNoticeBody(errClass, state, rc.runID, timeout)+autoPauseNoticeSuffix(rc.snap, pausedAfter)))
}

// deliverPauseNotice announces that the failure of a run with no per-run
// notice (a restart-orphaned sandbox run, an adopted turn) auto-paused its job.
// The target is resolved from a fresh snapshot of the now-paused job.
func (s *Scheduler) deliverPauseNotice(rc runCtx, errClass ErrorClass, state RunState, timeout time.Duration, paused int) {
	snap, ok := s.tbl.runSnapshot(rc.jobID)
	if !ok {
		return
	}
	rc.snap = snap
	rc.notifyTo = s.resolveNotifyTarget(snap.platName, snap.chatID, snap.notifyPlat, snap.notifyChat, snap.notify)
	s.deliverFailureNotice(rc, errClass, state, timeout, paused)
}

// formatNoticeBudget renders d without Duration.String's zero tails
// ("5m", "1h", "1m30s" rather than "5m0s", "1h0m0s").
func formatNoticeBudget(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d >= time.Minute && d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	default:
		return d.String()
	}
}

// labelOrID returns the IM-notice display label: snap.label when populated,
// jobID otherwise, so the "[Cron <X>] …" prefix stays readable when both
// Title and Prompt are empty.
func (s jobSnapshot) labelOrID() string {
	if s.label != "" {
		return s.label
	}
	return s.jobID
}

// snapshotJobLocked copies the fields a run works from; callers MUST hold
// s.tbl.mu (read or write). jobTable's runSnapshot / runSnapshotIfLive are its
// callers. A free function rather than a method so the dependency on the
// caller's lock is explicit and the helper cannot re-acquire it.
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
		sideEffects:   j.SideEffects != nil && *j.SideEffects,
		lastSessionID: j.LastSessionID,
	}
	// Alias j.Notify instead of deep-copying (#1931): UpdateJob only ever
	// *reassigns* j.Notify to a fresh pointer under s.tbl.mu.Lock, never mutates
	// *j.Notify in place, so the pointed-to bool is immutable once published
	// and the sole reader (resolveNotifyDecision) only nil-checks and derefs.
	// Aliasing is therefore alloc-free and tear-free.
	snap.notify = j.Notify
	return snap
}
