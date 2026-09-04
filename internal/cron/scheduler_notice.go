// scheduler_notice.go: cron IM-notice formatting (notice-prefix consts +
// formatCronNotice + escapeCronMarkdownPunct) and the jobSnapshot that feeds
// notice labels. None read s.stopCtx; methods stay on *Scheduler / jobSnapshot
// so private fields remain accessible without exporting.

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
// for the next tick rather than racing the in-flight result.
//
// 字段按 size DESC 排，消除 string/bool/*bool 混排引入的 padding。
type jobSnapshot struct {
	prompt  string
	workDir string
	jobID   string
	// label is the human-readable title for IM notice prefixes, computed via
	// jobTitleOrFallback under s.mu so a concurrent SetJobPrompt cannot tear
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
	// 的 stub-refresh 闭包直接调 registerStubByValue，不再回头加 s.mu 读。失败
	// 路径用 snap-time chain anchor，后续新成功 run 由其 finishRun 路径再覆写。
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

// labelOrID returns the IM-notice display label: snap.label when populated,
// jobID otherwise, so the "[Cron <X>] …" prefix stays readable when both
// Title and Prompt are empty.
func (s jobSnapshot) labelOrID() string {
	if s.label != "" {
		return s.label
	}
	return s.jobID
}

// snapshotJob reads j under s.mu.RLock so a concurrent SetJobPrompt /
// UpdateJob cannot tear the read across fields. Always returns a value; j is
// dereferenced inside the lock.
//
// LOCK: Must NOT be called while s.mu is already held — acquires s.mu.RLock
// internally. robfig/cron callbacks must never hold s.mu when invoking it.
func (s *Scheduler) snapshotJob(j *Job) jobSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return snapshotJobLocked(j)
}

// snapshotJobLocked is the lock-held variant of snapshotJob: callers MUST hold
// s.mu (read or write). executeOpt's jitter-window block uses it to fold the
// post-jitter `cur.Paused` recheck and the snapshot copy into a single RLock
// window (#1351). A free function rather than a method so the dependency on
// the caller's lock is explicit and the helper cannot re-acquire it.
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
	// *reassigns* j.Notify to a fresh pointer under s.mu.Lock, never mutates
	// *j.Notify in place, so the pointed-to bool is immutable once published
	// and the sole reader (resolveNotifyDecision) only nil-checks and derefs.
	// Aliasing is therefore alloc-free and tear-free.
	snap.notify = j.Notify
	return snap
}
