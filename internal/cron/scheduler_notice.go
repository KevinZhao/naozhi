// scheduler_notice.go: cron IM-notice formatting (notice-prefix consts +
// formatCronNotice + escapeCronMarkdownPunct + failureNoticeBody) and the
// jobSnapshot that feeds notice labels and hints. None read s.stopCtx; methods stay on *Scheduler / jobSnapshot
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

// failureNoticeBody is the IM body, sent to `to`, for a run of snap's job that
// did not succeed: the cause named from errClass/state (and turnCause for a
// failed turn), then the run id's first 8 hex chars (the same short form the
// dashboard history shows). timeout is the run's wall-clock budget, printed on
// the timed-out bodies. Never carries the raw error text.
func failureNoticeBody(snap jobSnapshot, to NotifyTarget, errClass ErrorClass, turnCause TurnCause, state RunState, runID string, timeout time.Duration) string {
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
	case errClass == ErrClassTurnFailed && turnCause == TurnCauseContextTooLong && !snap.fresh:
		cause = contextTooLongPersistentNotice(snap, to)
	case errClass == ErrClassTurnFailed:
		cause = turnFailedNotice(turnCause)
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

// turnFailedNotices words each TurnCause for a cron notice. Unlike the chat
// replies for the same causes they never suggest 继续 or /new: in the notify
// chat those would act on that chat's session, not on the job's.
var turnFailedNotices = map[TurnCause]string{
	TurnCauseMaxTurns:           "执行未完成（已达到最大执行步数），请检查执行历史",
	TurnCauseBudget:             "执行未完成（已达到费用上限），请检查执行历史",
	TurnCauseRefused:            "执行失败（模型拒绝了本次请求），请检查执行历史",
	TurnCauseTruncated:          "执行未完成（回复超出模型单次输出上限），请检查执行历史",
	TurnCauseContextTooLong:     "执行失败（对话上下文已超出模型上限），请检查执行历史",
	TurnCauseQuota:              "执行失败（API 额度已用尽），请联系管理员",
	TurnCausePermission:         "执行失败（请求被拒绝：权限或内容策略），请联系管理员",
	TurnCauseBackendOverloaded:  "执行失败（后端服务负载较高），请检查执行历史",
	TurnCauseBackendRateLimited: "执行失败（后端调用过于频繁），请检查执行历史",
	TurnCauseBackendAuth:        "执行失败（后端认证失败或凭证已过期），请联系管理员",
	TurnCauseBackendInvalid:     "执行失败（后端无法处理本次请求），请检查执行历史",
	TurnCauseBackendUnreachable: "执行失败（连接模型服务超时或网络异常），请检查执行历史",
	TurnCauseResumeUnavailable:  "执行失败（上次会话无法恢复），下次执行将尝试开启新会话",
	TurnCauseCLIConfig:          "执行失败（CLI 配置错误导致启动失败，如 MCP 配置无效），请联系管理员",
	TurnCauseCLIMissingRuntime:  "执行失败（CLI 运行环境缺失），请联系管理员",
}

// turnFailedNotice is the notice cause for a failed turn; a cause nobody
// named gets the generic sentence.
func turnFailedNotice(c TurnCause) string {
	if s, ok := turnFailedNotices[c]; ok {
		return s
	}
	return "执行失败（后端报告本轮出错），请检查执行历史"
}

// contextTooLongPersistentNotice is the context-too-long cause, sent to `to`,
// for a job that keeps its context: every later run resumes the same oversized
// conversation, so it names the dashboard toggle that resets it. An IM job also
// gets the /cron mode command, placed in the creating chat when the notice
// lands elsewhere (/cron mode only works there).
func contextTooLongPersistentNotice(snap jobSnapshot, to NotifyTarget) string {
	const head = "执行失败（对话上下文已超出模型上限）；该任务保留上下文，之后每次执行都会因此失败，可在控制台编辑任务勾选“每次全新上下文”"
	switch {
	case !snap.hasIMChat():
		return head
	case snap.isSourceChat(to):
		return head + "，或发送 /cron mode " + snap.jobID + " fresh 改为每次从新会话开始"
	default:
		return head + "，或在创建该任务的会话发送 /cron mode " + snap.jobID + " fresh"
	}
}

// autoPauseNoticeSuffix is appended to a failure notice sent to `to` when that
// run's failure auto-paused the job; pausedAfter is finishRun's result (0 = not
// paused). It says where to resume: /cron resume only works in the job's own
// chat, so a notice delivered elsewhere names that chat generically (never its
// id) and the dashboard; a job with no IM chat resumes from the dashboard.
func autoPauseNoticeSuffix(snap jobSnapshot, to NotifyTarget, pausedAfter int) string {
	if pausedAfter <= 0 {
		return ""
	}
	var how string
	switch {
	case !snap.hasIMChat():
		how = "修复后在控制台恢复"
	case snap.isSourceChat(to):
		how = "修复后发送 /cron resume " + snap.jobID + " 恢复"
	default:
		how = "修复后在创建该任务的会话发送 /cron resume " + snap.jobID + "，或在控制台恢复"
	}
	return "；已连续失败 " + strconv.Itoa(pausedAfter) + " 次，任务已自动暂停，" + how
}

// deliverFailureNotice sends the IM notice for a run of rc that did not
// succeed: failureNoticeBody plus, when pausedAfter > 0, the auto-pause
// sentence.
func (s *Scheduler) deliverFailureNotice(rc runCtx, errClass ErrorClass, turnCause TurnCause, state RunState, timeout time.Duration, pausedAfter int) {
	s.deliverNotice(rc.notifyTo, formatCronNotice(rc.snap.labelOrID(),
		failureNoticeBody(rc.snap, rc.notifyTo, errClass, turnCause, state, rc.runID, timeout)+autoPauseNoticeSuffix(rc.snap, rc.notifyTo, pausedAfter)))
}

// deliverPauseNotice announces that the failure of a run with no per-run
// notice (an adopted turn) auto-paused its job.
// The target is resolved from a fresh snapshot of the now-paused job.
func (s *Scheduler) deliverPauseNotice(rc runCtx, errClass ErrorClass, turnCause TurnCause, state RunState, timeout time.Duration, paused int) {
	snap, ok := s.tbl.runSnapshot(rc.jobID)
	if !ok {
		return
	}
	rc.snap = snap
	rc.notifyTo = s.resolveNotifyTarget(snap.platName, snap.chatID, snap.notifyPlat, snap.notifyChat, snap.notify)
	s.deliverFailureNotice(rc, errClass, turnCause, state, timeout, paused)
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

// hasIMChat reports whether the job was created in an IM chat, the only place
// its /cron commands work; dashboard jobs and jobs with no source chat are
// managed from the dashboard alone.
func (s jobSnapshot) hasIMChat() bool {
	return s.platName != "dashboard" && s.platName != "" && s.chatID != ""
}

// isSourceChat reports whether to is the chat the job was created in.
func (s jobSnapshot) isSourceChat(to NotifyTarget) bool {
	return to == NotifyTarget{Platform: s.platName, ChatID: s.chatID}
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
