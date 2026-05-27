package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/osutil"
)

// Bounds for notify target fields set by authenticated dashboard users. The
// platform must match a known IM provider to avoid silent notification drops
// (misspelt names used to fall through); chat_id length is capped so a user
// cannot stuff megabytes into cron_jobs.json via a single API call.
var validNotifyPlatforms = map[string]struct{}{
	"":        {}, // empty = fall back to cron.notify_default
	"feishu":  {},
	"slack":   {},
	"discord": {},
	"weixin":  {},
}

const maxNotifyChatIDLen = 256

// Cron input bounds shared with the IM `/cron` path. Both surfaces feed the
// same on-disk cron_jobs.json schema, so the limits must stay in lockstep —
// see internal/cron/limits.go. R216-CR-1.
const (
	maxCronPromptBytesDashboard   = cron.MaxPromptBytes
	maxCronIDLenDashboard         = cron.MaxIDLen
	maxCronScheduleBytesDashboard = cron.MaxScheduleBytes
)

// maxCronWorkDirBytesDashboard caps the raw work_dir string before it reaches
// validateWorkspace. Even absolute paths rarely exceed 1 KiB on Linux
// (PATH_MAX is typically 4096), so 1024 is generous. Without this guard a
// multi-MB work_dir body would be echoed into slog attrs via the debug-log
// on validation failure, allowing log-flood from an authenticated attacker.
const maxCronWorkDirBytesDashboard = 1024

// stringFieldPolicy carries the per-field knobs for validateStringField:
// what to call this field in error messages, whether Tab/LF are accepted,
// and whether the three failure classes ("invalid characters" / "invalid
// control characters" / "invalid unicode control characters") collapse
// into a single error label. Centralising these knobs keeps the security
// policy in one place — every cron-edge field shares the same UTF-8 + C0
// + IsLogInjectionRune three-pass scan, so a future safety change touches
// validateStringField alone instead of five copy-pasted loops. R219-CR-5.
type stringFieldPolicy struct {
	// name is the wire-visible field label embedded in error messages.
	name string
	// allowTab whitelists 0x09 in the byte scan (cron prompt / title body).
	allowTab bool
	// allowLF whitelists 0x0a in the byte scan (cron prompt only — cron
	// schedules and absolute paths cannot legally contain a newline).
	allowLF bool
	// disallowLF reports LF / CR as "<name> must be a single line" instead
	// of folding them into the generic "invalid control characters" branch.
	// Used by single-line fields (Job.Title) where the UI specifically
	// requires "no embedded newline" and benefits from a distinct error
	// message rather than a generic control-character class. Mutually
	// exclusive with allowLF — setting both is a programmer error and
	// allowLF wins (LF is whitelisted, disallowLF cannot fire). R239-CR-4.
	disallowLF bool
	// collapseErrors maps every failure class onto "<name> contains invalid
	// characters" instead of the WorkDir/Prompt-style three-tier messages.
	// True for notify_chat_id and schedule (where API consumers historically
	// only see one error string); false for work_dir / prompt where the
	// distinction between "control byte" and "bidi rune" carries audit
	// signal.
	collapseErrors bool
}

// validateStringField runs the three-pass UTF-8 → C0+DEL byte → log-injection
// rune scan that every cron-handler-edge user-controlled string requires.
// The caller owns the length check (units differ: WorkDir/Prompt cap bytes,
// Title caps runes, NotifyChatID caps bytes) and any field-specific extras
// (validateCronWorkDir's filepath.IsAbs check). R219-CR-5.
//
// R250-PERF-22 (#1125): the IsLogInjectionRune set covers C1 (0x80..0x9F)
// and assorted Unicode formatting codepoints (bidi, LS/PS) which all
// encode in UTF-8 with at least one byte >= 0x80. An ASCII-only string —
// the common case for absolute paths, schedule expressions, lowercase-hex
// IDs — therefore cannot contain a hit, and the second `for _, r := range
// s` decode pass is pure overhead. Track an `anyHighBit` flag during the
// first byte loop and skip the rune walk when the input is provably
// ASCII. Hot path on cron CREATE/PATCH validates 5+ fields per request.
func validateStringField(s string, p stringFieldPolicy) error {
	// R179-GO-P1: validate UTF-8 before the rune-range loop below. A
	// `for _, r := range s` over broken UTF-8 silently produces utf8.RuneError
	// (U+FFFD) for each invalid byte, which IsLogInjectionRune does not flag
	// — this lets a crafted string with lone continuation bytes smuggle
	// arbitrary bytes into cron_jobs.json / WS broadcasts. Mirrors
	// validateProjectName.
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s contains invalid characters", p.name)
	}
	anyHighBit := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 {
			anyHighBit = true
			continue
		}
		if c >= 0x20 && c != 0x7f {
			continue
		}
		if c == '\t' && p.allowTab {
			continue
		}
		if c == '\n' && p.allowLF {
			continue
		}
		// R239-CR-4: surface single-line violations (LF / CR) with a
		// distinct error message when disallowLF is set, instead of
		// folding them into the generic control-character class. UI
		// fields like Job.Title surface this directly to the operator.
		if p.disallowLF && (c == '\n' || c == '\r') {
			return fmt.Errorf("%s must be a single line", p.name)
		}
		if p.collapseErrors {
			return fmt.Errorf("%s contains invalid characters", p.name)
		}
		return fmt.Errorf("%s contains invalid control characters", p.name)
	}
	// R250-PERF-22 (#1125): pure-ASCII fast path — IsLogInjectionRune cannot
	// match any rune whose UTF-8 form is single-byte < 0x80, so the rune
	// loop below has zero work to do when no high-bit byte was observed.
	// Skips the second UTF-8 decode pass on the common case (absolute
	// paths, cron schedules, lowercase-hex IDs).
	if !anyHighBit {
		return nil
	}
	// Reject Unicode bidi override / embedding / directional isolate
	// characters (U+202A–U+202E, U+2066–U+2069) and Unicode line/paragraph
	// separators (U+2028/U+2029) which encode as valid UTF-8 sequences with
	// all bytes >= 0x20 and therefore pass the byte loop above. These
	// characters can flip terminal rendering and corrupt log pipelines that
	// use U+2028 as a line boundary. Matches the filter applied by
	// sanitizeKeyComponent in the session package so cron fields and
	// session-key fields reject the same log-injection class uniformly.
	for _, r := range s {
		if osutil.IsLogInjectionRune(r) {
			if p.collapseErrors {
				return fmt.Errorf("%s contains invalid characters", p.name)
			}
			return fmt.Errorf("%s contains invalid unicode control characters", p.name)
		}
	}
	return nil
}

// validateCronWorkDir rejects work_dir strings with embedded control
// characters that would corrupt slog attribute logging (ANSI injection into
// structured logs, CR/LF line-wrapping into log pipelines). Length check
// matches prompt/schedule guards so all three fields reject the same class
// of log-injection payloads at the handler edge, before validateWorkspace
// sees them.
//
// R172-SEC-L1: relative paths are rejected up front so the cron edge
// boundary does not depend on validateWorkspace to fail on "." / "foo/bar"
// later. Defense-in-depth: if validateWorkspace ever loosens its IsAbs
// check (e.g. to accept workspace-relative paths for a new feature) the
// cron handler continues to enforce the stricter contract inherited from
// the scheduler worker which runs on absolute paths only.
func validateCronWorkDir(wd string) error {
	if len(wd) > maxCronWorkDirBytesDashboard {
		return fmt.Errorf("work_dir exceeds %d-byte limit", maxCronWorkDirBytesDashboard)
	}
	if err := validateStringField(wd, stringFieldPolicy{name: "work_dir"}); err != nil {
		return err
	}
	if !filepath.IsAbs(wd) {
		return fmt.Errorf("work_dir must be an absolute path")
	}
	return nil
}

// validateNotifyTarget enforces platform allowlist + chat_id size bound.
// R177-SEC-7: additionally reject C0/C1/bidi/LS/PS runes so a crafted
// chat_id cannot land log-injection bytes in persisted cron_jobs.json
// or forge structure in the /api/cron WS broadcast.
func validateNotifyTarget(platform, chatID string) error {
	if _, ok := validNotifyPlatforms[platform]; !ok {
		return fmt.Errorf("invalid notify_platform")
	}
	if len(chatID) > maxNotifyChatIDLen {
		return fmt.Errorf("notify_chat_id exceeds %d-byte limit", maxNotifyChatIDLen)
	}
	return validateStringField(chatID, stringFieldPolicy{name: "notify_chat_id", collapseErrors: true})
}

// validateCronScheduleChars rejects C0/C1/bidi/LS/PS runes in a cron
// schedule expression before it reaches robfig/cron's parser. robfig
// does not scrub its input, so log lines like `slog.Debug("cron
// preview parse failed", "err", err)` would forward unescaped bidi
// overrides into operator logs. Authenticated-only endpoint so the
// CVSS is low, but this keeps the log-injection posture consistent
// across every user-controlled string entering scheduler paths.
// R177-SEC-9.
func validateCronScheduleChars(schedule string) error {
	return validateStringField(schedule, stringFieldPolicy{name: "schedule", collapseErrors: true})
}

// cronRunSummaryView is the JSON shape for a single cron run summary.
// Shared between handleList (recent-run preview embedded in the cron
// dashboard list) and handleRunsList (per-job paginated history).
// Both wire shapes must stay in lockstep, so the type is package-level
// rather than declared twice in handler bodies.
type cronRunSummaryView struct {
	RunID      string `json:"run_id"`
	State      string `json:"state"`
	Trigger    string `json:"trigger,omitempty"`
	StartedAt  int64  `json:"started_at"`
	EndedAt    int64  `json:"ended_at,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	ErrorClass string `json:"error_class,omitempty"`
}

// cronSummaryToView projects a cron.CronRunSummary into the wire shape used
// by handleList's recent_runs preview and handleRunsList's paginated history.
// R233-CR-3: the two callers had identical conversion blocks and would
// silently diverge if a future field were added in only one place.
func cronSummaryToView(r cron.CronRunSummary) cronRunSummaryView {
	row := cronRunSummaryView{
		RunID:      r.RunID,
		State:      string(r.State),
		Trigger:    string(r.Trigger),
		StartedAt:  r.StartedAt.UnixMilli(),
		DurationMS: r.DurationMS,
		SessionID:  r.SessionID,
		ErrorClass: string(r.ErrorClass),
	}
	if !r.EndedAt.IsZero() {
		row.EndedAt = r.EndedAt.UnixMilli()
	}
	return row
}

// cronListResp is the wire shape returned by GET /api/cron — the dashboard
// list view. R230B-CR-3 swapped the previous map[string]any literal for
// this named struct so the JSON encoder can cache the type's reflect
// descriptor across the 1-Hz dashboard polls instead of paying the
// per-call map iteration + interface boxing each request.
type cronListResp struct {
	Jobs          []cronJobView          `json:"jobs"`
	Timezone      string                 `json:"timezone"`
	TimezoneLabel string                 `json:"timezone_label"`
	TimezoneAbbr  string                 `json:"timezone_abbr"`
	NotifyDefault *cronNotifyDefaultView `json:"notify_default,omitempty"`
}

// cronCreateResp is the wire shape returned by POST /api/cron — just the
// new job ID. R242-CR-10 promoted this from an inline map[string]any so
// the JSON encoder can cache the reflect descriptor and the field name
// is enforced at compile time.
//
// Wire-only consumer: dashboard.js cronCreateJob reads only resp.id;
// adding fields requires updating the JS reader as well.
type cronCreateResp struct {
	ID string `json:"id"`
}

// cronCurrentRunView is the inline running-run summary embedded in
// cronJobView. Only set when the scheduler reports an in-flight run for
// the corresponding job at list time.
type cronCurrentRunView struct {
	RunID     string `json:"run_id"`
	StartedAt int64  `json:"started_at"`
	Phase     string `json:"phase,omitempty"`
	Trigger   string `json:"trigger,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// cronRunCountersView mirrors cron.RunCounters for the dashboard. Field
// order tracks RunCounters so JSON wire keys cannot diverge silently.
type cronRunCountersView struct {
	Total     int64 `json:"total,omitempty"`
	Succeeded int64 `json:"succeeded,omitempty"`
	Failed    int64 `json:"failed,omitempty"`
	Skipped   int64 `json:"skipped,omitempty"`
	TimedOut  int64 `json:"timed_out,omitempty"`
	Canceled  int64 `json:"canceled,omitempty"`
}

// cronRunDetailView is the JSON shape returned by GET /api/cron/{job}/runs/{run}.
// Promoted from a handler-body anonymous struct (R239-CR-9) for parity with
// cronRunSummaryView / cronJobView / cronRunCountersView — a package-level
// type can be referenced from tests and any future helper that constructs
// the same payload, instead of being trapped inside the handler closure.
type cronRunDetailView struct {
	RunID       string `json:"run_id"`
	JobID       string `json:"job_id"`
	State       string `json:"state"`
	Trigger     string `json:"trigger,omitempty"`
	StartedAt   int64  `json:"started_at"`
	EndedAt     int64  `json:"ended_at,omitempty"`
	DurationMS  int64  `json:"duration_ms,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	WorkDir     string `json:"work_dir,omitempty"`
	Fresh       bool   `json:"fresh,omitempty"`
	Result      string `json:"result,omitempty"`
	ResultBytes int    `json:"result_bytes,omitempty"`
	ErrorClass  string `json:"error_class,omitempty"`
	ErrorMsg    string `json:"error_msg,omitempty"`
}

// cronJobView is the per-job element inside cronListResp.Jobs. Promoted to
// package-level (R230B-CR-3) so cronListResp can reference it; the previous
// inline-typed declaration kept it locked inside handleList.
type cronJobView struct {
	ID             string `json:"id"`
	Schedule       string `json:"schedule"`
	Prompt         string `json:"prompt"`
	Title          string `json:"title,omitempty"`
	Platform       string `json:"platform"`
	ChatID         string `json:"chat_id"`
	CreatedBy      string `json:"created_by,omitempty"`
	CreatedAt      int64  `json:"created_at"`
	Paused         bool   `json:"paused"`
	WorkDir        string `json:"work_dir,omitempty"`
	NotifyPlatform string `json:"notify_platform,omitempty"`
	NotifyChatID   string `json:"notify_chat_id,omitempty"`
	LastResult     string `json:"last_result,omitempty"`
	LastRunAt      int64  `json:"last_run_at,omitempty"`
	LastError      string `json:"last_error,omitempty"`
	// LastErrorClass 是 P0 cron-run-history 的机器可读错误分类。前端用它
	// 选图标/色板而非 substring-grep LastError。空 = 无错误 / 旧 job。
	LastErrorClass string `json:"last_error_class,omitempty"`
	NextRun        int64  `json:"next_run,omitempty"`
	// Notify is a pointer so the view preserves the tri-state (nil vs
	// explicit true/false). nil renders as "legacy default" on the client.
	Notify       *bool `json:"notify,omitempty"`
	FreshContext bool  `json:"fresh_context,omitempty"`
	// Missed / MissedSince: cron-v2-polish §3.3 Increment C.
	// missed=true 表示进程休眠 / 重启空窗期该 job 错过了至少一次调度。
	// MissedSince 是"按 schedule 算上一次应跑的毫秒时刻"，UI 可以用来
	// 显示 "上次应跑于 …"。未 missed 时两个字段都省略。
	Missed      bool  `json:"missed,omitempty"`
	MissedSince int64 `json:"missed_since,omitempty"`
	// CurrentRun: P0 — 仅 job 正在执行时存在，前端据此显示"运行中 Xs"。
	CurrentRun *cronCurrentRunView `json:"current_run,omitempty"`
	// Stats: P0 — 累计执行计数；P1 引入 avg_ms / p95_ms 时不动 wire shape。
	Stats *cronRunCountersView `json:"stats,omitempty"`
	// RecentRuns: P1 cron-run-history — newest-first 摘要数组；只在卡片
	// 折叠态做 hover-tooltip 状态气泡。空 = 此 job 尚无持久化历史
	// （新建 / 历史已被 GC 清空 / StorePath 为空）。
	RecentRuns []cronRunSummaryView `json:"recent_runs,omitempty"`
	// Backend: per docs/rfc/multi-backend.md §9 cron RPC contract. ""
	// 表示跟随 router default；前端编辑器据此回填 backend 下拉选项。
	Backend string `json:"backend,omitempty"`
	// PromptTruncated is set by the GET /api/cron compact mode when the
	// Prompt above was clipped to compactPromptPrefixBytes. The dashboard
	// uses this flag to decide whether the cached client-side prompt needs
	// a refetch before opening the editor / drawer-detail view. R236-SEC-08
	// (#494): exists only on the compact path, so a non-compact list keeps
	// byte-equal wire shape with prior releases.
	PromptTruncated bool `json:"prompt_truncated,omitempty"`
}

// compactPromptPrefixBytes is the upper bound on Prompt bytes returned by
// GET /api/cron?compact=1. 256 bytes is the cap chosen in R236-SEC-08
// (#494): large enough to keep a one-line title-style summary visible in
// any cached client view, small enough that 50 jobs × 256 B = 12 KiB
// total per poll instead of the prior 50 × 8 KiB = 400 KiB worst case.
//
// Truncation is done in handleList against the rendered byte length, but
// we clip on a UTF-8 boundary so a multi-byte rune at the cap can't end
// up half-decoded by JSON consumers. Tests pin this contract — see
// TestHandleList_Compact_TruncatesPromptOnRune.
const compactPromptPrefixBytes = 256

// truncatePromptUTF8 returns prompt with no more than max bytes, clipped
// at the most recent UTF-8 rune boundary so the truncated string is still
// valid UTF-8. Returns (clipped, true) when truncation occurred so the
// caller can stamp PromptTruncated; otherwise returns (prompt, false).
func truncatePromptUTF8(prompt string, max int) (string, bool) {
	if max <= 0 || len(prompt) <= max {
		return prompt, false
	}
	// Walk back from `max` until we land on a leading UTF-8 byte (top two
	// bits are not 10xxxxxx). Bounded by ≤4 byte step-back per UTF-8
	// invariant — never re-scans the whole prompt.
	for n := max; n > 0; n-- {
		if utf8.RuneStart(prompt[n]) {
			return prompt[:n], true
		}
	}
	// All-continuation prefix is impossible for valid UTF-8 input but
	// fall back defensively rather than return invalid bytes.
	return "", true
}

// cronNotifyDefaultView mirrors the {platform, chat_id} pair previously
// rendered as a nested map[string]string. Pointer-typed in cronListResp so
// the omitempty actually drops the key when no default is configured.
type cronNotifyDefaultView struct {
	Platform string `json:"platform"`
	ChatID   string `json:"chat_id"`
}

// cronRunsListResp is the wire shape returned by GET /api/cron/runs —
// per-job paginated history. NextBefore is omitted when the page is
// partial, matching the previous map[string]any behaviour.
type cronRunsListResp struct {
	Runs       []cronRunSummaryView `json:"runs"`
	NextBefore int64                `json:"next_before,omitempty"`
}

// cronPreviewResp is the wire shape returned by GET /api/cron/preview.
// Replaces an inline map[string]any so the JSON encoder can cache the
// reflect descriptor and field names are enforced at compile time
// (R250-CR-25). The "valid: false" branch carries only Error; the
// success branch carries Timezone (+ optional TimezoneLabel) and, when
// the parser produced runs, NextRun and NextRuns. omitempty keeps the
// on-the-wire shape identical to the previous map[string]any output —
// dashboard.js cronPreviewJob still reads `valid` / `error` /
// `timezone` / `timezone_label` / `next_run` / `next_runs` unchanged.
type cronPreviewResp struct {
	Valid         bool    `json:"valid"`
	Error         string  `json:"error,omitempty"`
	Timezone      string  `json:"timezone,omitempty"`
	TimezoneLabel string  `json:"timezone_label,omitempty"`
	NextRun       int64   `json:"next_run,omitempty"`
	NextRuns      []int64 `json:"next_runs,omitempty"`
}

// cronUpdateResp is the wire shape returned by PATCH /api/cron. Replaces
// an inline map[string]any so the field names are compile-checked
// (R250-CR-24). Status is always "ok" on the success path; ID echoes the
// canonical job ID resolved by UpdateJob.
type cronUpdateResp struct {
	Status string `json:"status"`
	ID     string `json:"id"`
}

// validateCronBackend enforces the shared shape contract for the
// dashboard-picked backend override on cron jobs:
//   - empty is OK (router default fallback at execute time);
//   - length <= maxBackendIDLen bytes;
//   - charset matches isValidBackendID (R233-SEC-9 unification).
//
// Unknown backend IDs are NOT rejected here — the session router's
// wrapperFor clamps unknowns to the configured default so the cron job
// keeps running rather than failing every tick because the operator
// removed a backend from config.yaml. This handler-edge gate stops only
// shape-invalid input that would otherwise pollute logs / persisted JSON.
//
// R233-SEC-9: previously used a tighter [a-z0-9_-] subset, leaving
// uppercase / '.' allowed by the WS isValidBackendID path and rejected
// here. Now both paths share isValidBackendID + maxBackendIDLen so a
// caller cannot confuse the two surfaces. The relaxation is forward-
// compatible: any backend ID validated under the older subset still
// satisfies isValidBackendID's superset.
func validateCronBackend(backend string) error {
	if backend == "" {
		return nil
	}
	if len(backend) > maxBackendIDLen {
		return fmt.Errorf("backend exceeds %d-byte limit", maxBackendIDLen)
	}
	if !isValidBackendID(backend) {
		// R230-CQ-12: error string aligned with send.handleSend's
		// dashboard-side gate so dashboard JS / external API consumers
		// see one message regardless of which surface rejected.
		return fmt.Errorf("invalid backend identifier")
	}
	return nil
}

// validateCronPrompt rejects prompts larger than the dashboard cap or
// containing control characters. Cron prompts are delivered via stdin as a
// stream-json user message (cron/scheduler.go → session.Send → NewUserMessage),
// where json.Marshal escapes embedded \n so NDJSON framing stays intact. LF is
// therefore allowed to support multi-paragraph playbook prompts. CR is still
// rejected because `tail -f` / `journalctl` treat it as a carriage return that
// overwrites the current log line — a log-poisoning surface unrelated to
// framing. null bytes remain forbidden (execve silently truncates at the first
// NUL). Tab is allowed because prompts may indent examples.
//
// Unlike project_api.handleConfigPut's planner_prompt guard, cron prompts do
// not end up in argv — planner_prompt and scratch context still flow into
// `--append-system-prompt` and must stay single-line; do not copy this relaxed
// policy back to those fields without re-auditing their downstream writers.
//
// Second pass mirrors validateCronWorkDir: reject C1 controls + Unicode
// bidi / directional isolate / line separator runes that are >= 0x20 at
// the byte level and therefore bypass the ASCII loop above.
// validateCronTitle 是 Job.Title 在 handler 层的守门：单行（禁内嵌换行，
// 卡片布局不允许）、长度 256 rune、禁控制字符 + 日志注入 rune。空值合法
// （允许用户不填，UI 自动 fallback 到 Prompt 首行）。
// 与 validateCronPrompt 一致的清洗集，只多禁换行。
//
// R239-CR-4: 通过 stringFieldPolicy{disallowLF: true} 复用 validateStringField
// 共享的 25 行 C0+IsLogInjectionRune 扫描，不再维护独立 loop。Tab 仍 allow，
// 长度仍按 rune 计（与 validateStringField 的 byte 长度独立处理在外层）。
func validateCronTitle(title string) error {
	if title == "" {
		return nil
	}
	if n := utf8.RuneCountInString(title); n > cron.MaxCronTitleLen {
		return fmt.Errorf("title exceeds %d-rune limit", cron.MaxCronTitleLen)
	}
	return validateStringField(title, stringFieldPolicy{name: "title", allowTab: true, disallowLF: true})
}

// validateCronPrompt allows Tab and LF (multi-paragraph playbooks) but
// stringFieldPolicy with allowLF=true still rejects \r (CR). That asymmetry
// is intentional: prompts are written into cron_jobs.json as JSON-quoted
// strings (json.Marshal escapes \n inside the quoted value, so NDJSON
// framing on the wire stays intact), but a bare CR would still survive the
// JSON encode and later corrupt `tail -f` / `journalctl` views by carriage-
// returning over the previous log line — a log-poisoning surface unrelated
// to wire framing. There is no legitimate reason for an authored prompt to
// contain CR (Linux line endings are LF, dashboard textareas normalise
// CRLF→LF before submit), so rejecting it here is cheap defence-in-depth
// matching validateCronTitle's explicit '\r' branch. R230-CQ-19.
func validateCronPrompt(prompt string) error {
	if len(prompt) > maxCronPromptBytesDashboard {
		return fmt.Errorf("prompt exceeds %d-byte limit", maxCronPromptBytesDashboard)
	}
	return validateStringField(prompt, stringFieldPolicy{name: "prompt", allowTab: true, allowLF: true})
}

// CronHandlers groups the cron job management API endpoints.
type CronHandlers struct {
	scheduler   *cron.Scheduler
	allowedRoot string
	// claudeDir is the absolute path to ~/.claude. Used by handleRunTranscript
	// to locate the JSONL conversation file for a given run's session_id.
	// Empty disables the transcript endpoint (returns fallback:"missing").
	// cron-dashboard-redesign P2a §4.4.3.
	claudeDir string
	// runsLimiter caps how often a single authenticated caller can hit
	// `/api/cron/runs`, `/api/cron/runs/{run_id}`, and the transcript
	// endpoint. All three fan out filesystem I/O against the per-job runs
	// directory or the JSONL on disk; an attacker holding a stolen dashboard
	// token can otherwise enumerate the entire run history at unbounded
	// rate. R222-SEC-3.
	//
	// Nil-guarded so tests built via newCronHandlersForTest (and other
	// hand-rolled CronHandlers instances) skip the gate; wiring lives in
	// server.New.
	runsLimiter *ipLimiter

	// listLimiter caps GET /api/cron — the dashboard's primary 1 Hz
	// polling endpoint. R242-CR-3: the runs/transcript endpoints are
	// already rate-limited because each call fans out FS I/O, but the
	// list endpoint runs ListAllJobsWithNextRun + RecentRuns(5) per job
	// and per-call cost grows with N (jobs) × 5 (recent run files). A
	// stolen token can otherwise burn IO at unbounded rate while the
	// runs limiter sits idle.
	//
	// Sustained 2 req/s with burst 30 — generous enough that a single
	// dashboard tab refresh storm (open + immediate filter change at
	// the top of a minute) doesn't trip, but caps a parallel-poll
	// attacker at ~2 req/s steady-state per source IP. Mirrors the
	// shape of runsLimiter (rate.Every + burst) so ops familiarity
	// transfers; the higher steady rate reflects that this endpoint is
	// the dashboard's heartbeat.
	//
	// Nil-guarded just like runsLimiter so newCronHandlersForTest paths
	// skip the gate. Wiring lives in server.New.
	listLimiter *ipLimiter

	// writeLimiter caps per-IP rate of authenticated cron-write/control
	// endpoints that fan out side-effects beyond a cheap read:
	//
	//   - POST /api/cron/trigger  spawns the job's claude CLI subprocess
	//     and may send IM notifications, so loop-triggering is a realistic
	//     DoS / amplification vector even for a logged-in caller.
	//   - GET  /api/cron/preview   parses cron expressions in a tight loop
	//     up to N=10 next-run computations; cheaper than trigger but still
	//     not a heartbeat endpoint and shouldn't be unbounded.
	//
	// Sustained 30 req/min with burst 6 is generous for legitimate UI usage
	// (a single user form-edit cycle hits preview a handful of times per
	// minute) while capping a stolen-token attacker at one trigger every
	// 2 s steady-state. Single shared bucket per IP keeps the wiring
	// simple and the per-IP control surface uniform.
	//
	// Nil-guarded so newCronHandlersForTest paths skip the gate; wiring
	// lives in server.New. [R247-SEC-2 / R247-SEC-3]
	writeLimiter *ipLimiter

	// missedCache memoises HasMissedSchedule verdicts so the dashboard's
	// 1 Hz handleList path doesn't re-Parse the cron expression for every
	// job on every poll. Without the cache, robfig/cron's regexp NFA
	// build runs N (jobs) × T (parallel tabs) times per second; with it,
	// steady-state cost falls to N parses per missedCacheTTL because
	// cache hits skip Parse entirely. The verdict depends on (Schedule,
	// LastRunAt, startedAt) plus `now` modulo TTL — schedule edits and
	// scheduler restarts invalidate via the composite key (see the
	// missedScheduleVerdict helper); LastRunAt advances invalidate via
	// the lastRunNanos guard so a job that just ran does not keep an
	// outdated "missed" verdict for a full second. R245-PERF-4 (#857).
	missedCacheMu sync.Mutex
	missedCache   map[string]missedVerdict

	// transcriptSem caps concurrent /api/cron/runs/{run_id}/transcript
	// requests across the whole process. R243-SEC-12 (#798): each
	// in-flight transcript holds a 256 KB bufio.Scanner buffer plus
	// the LimitReader's 8 MB read budget, so the per-IP runsLimiter
	// alone is not enough — N distinct authenticated operators can
	// each saturate their own bucket and collectively park N×8 MB
	// of file-mapped pages plus N×256 KB of scanner buffers in
	// memory. The semaphore puts a process-wide ceiling on that
	// concurrency so memory cannot grow unbounded with operator
	// count. Excess requests receive 503 immediately, mirroring the
	// transcribeSemCap pattern in dashboard_transcribe.go. Nil leaves
	// the gate disabled (newCronHandlersForTest paths) so legacy
	// hand-rolled fixtures keep compiling.
	transcriptSem chan struct{}
}

// missedVerdict caches one HasMissedSchedule return tuple plus the inputs
// that decide whether the entry is still valid. Stored under
// CronHandlers.missedCache keyed by `jobID|schedule|startedNs` so a
// schedule edit (UpdateJob) or scheduler restart invalidates by key
// turnover (old keys become unreachable; the entry GCs away once the cap
// rotates). LastRunAt is intentionally NOT in the key — instead, it lives
// in the value as `lastRunNanos` so a tick that follows a fresh run
// triggers a recompute without growing the keyspace. R245-PERF-4 (#857).
type missedVerdict struct {
	missed       bool
	prevAt       time.Time
	lastRunNanos int64
	computedAt   time.Time
}

// missedCacheTTL is the freshness window for cached HasMissedSchedule
// verdicts. 1 s matches the dashboard poll cadence — verdicts that are
// up to one tick stale are equivalent to verdicts a parallel poller
// would have just computed, which is the same staleness the human eye
// sees on the rendered card anyway. R245-PERF-4 (#857).
const missedCacheTTL = time.Second

// missedCacheCap caps the cache size so a runtime UpdateJob storm (which
// turns over the (jobID, schedule, startedAt) key on every edit) cannot
// grow the map without bound. 2500 entries × ~120 bytes ≈ 300 KiB worst
// case — comfortably within budget for a heartbeat-path data structure
// and well above the practical N for a single naozhi instance. When the
// cap is hit we drop the entire map and let it rebuild; a sweep would
// pay map-iteration cost on a hot path for marginal benefit. R245-PERF-4
// (#857).
const missedCacheCap = 2500

// missedScheduleVerdict returns HasMissedSchedule(j, now, startedAt) but
// memoises the result for missedCacheTTL so the 1 Hz dashboard poll does
// not re-Parse the cron expression on every job per tick. Cache hits skip
// the regexp NFA build entirely; misses fall through to the cron package
// and store the freshly computed tuple for the next poller. Safe to call
// from concurrent goroutines (mu-protected map; map access is short and
// uncontended in practice because handleList serialises per request, but
// multiple parallel dashboard tabs each have their own request goroutine
// and may overlap). R245-PERF-4 (#857).
func (h *CronHandlers) missedScheduleVerdict(j *cron.Job, now, startedAt time.Time) (bool, time.Time) {
	if j == nil {
		return false, time.Time{}
	}
	startedNs := startedAt.UnixNano()
	key := j.ID + "|" + j.Schedule + "|" + strconv.FormatInt(startedNs, 10)
	lastRunNanos := j.LastRunAt.UnixNano()

	h.missedCacheMu.Lock()
	if h.missedCache != nil {
		if v, ok := h.missedCache[key]; ok {
			if v.lastRunNanos == lastRunNanos && now.Sub(v.computedAt) < missedCacheTTL {
				h.missedCacheMu.Unlock()
				return v.missed, v.prevAt
			}
		}
	}
	h.missedCacheMu.Unlock()

	missed, prevAt := cron.HasMissedSchedule(j, now, startedAt)

	h.missedCacheMu.Lock()
	if h.missedCache == nil || len(h.missedCache) >= missedCacheCap {
		// Lazy-init AND cap-reset use the same allocation: dropping the
		// whole map at the cap is cheaper than walking it to evict the
		// oldest entry, and the cap is large enough that a real workload
		// (jobs-on-disk × handful-of-tabs) will not approach it.
		h.missedCache = make(map[string]missedVerdict, 64)
	}
	h.missedCache[key] = missedVerdict{
		missed:       missed,
		prevAt:       prevAt,
		lastRunNanos: lastRunNanos,
		computedAt:   now,
	}
	h.missedCacheMu.Unlock()
	return missed, prevAt
}

// recentRunsPerJob is the per-job RecentRuns cap embedded in handleList's
// list response. Mirrors the literal previously inlined as
// scheduler.RecentRuns(j.ID, 5) so the bounded fan-out helper carries the
// same wire-shape contract the dashboard JS reads. Tooltip-bound; the
// dashboard's per-job runs detail drawer uses GET /api/cron/runs for
// richer pagination. R236-PERF-08 (#525).
const recentRunsPerJob = 5

// batchRecentRunsWorkers caps the concurrent RecentRuns goroutines spun up
// by batchRecentRuns. Sized 8 so a single 1 Hz dashboard poll on a 50-job
// install fans out across 8-way parallelism (≈ 6 jobs/worker, sub-ms wall
// time once the runStore cache is warm) without flooding sync.Map.Load
// contention or the Go scheduler with hundreds of short-lived goroutines.
// Above ~16 readers the per-jobLock TryLock + recentCacheEntry.mu acquire
// chain contends on Go runtime spinlocks and the marginal speedup goes
// negative; 8 stays comfortably below that knee. R236-PERF-08 (#525).
const batchRecentRunsWorkers = 8

// batchRecentRuns fans out scheduler.RecentRuns lookups across at most
// batchRecentRunsWorkers goroutines and returns one result per input job
// in input-index order. Used by handleList to drop the previous N×serial
// per-job lock acquire (the per-recentCacheEntry.mu chain) — under load,
// 1 Hz polls on a 50-job install no longer stall behind the slowest
// jobLock pass because at most W jobs queue at the runStore lock at any
// instant.
//
// Result slice is always len(jobs); entries may be nil for jobs with no
// run history. Caller is responsible for the nil-len check before
// projecting cron summaries onto the wire shape.
//
// Single-call cost vs. inline serial loop:
//   - Cold scheduler: serial = N × (warmCache + ringSnapshot); fan-out =
//     ⌈N/W⌉ × max(per-job warmCache); typical 4-8× speedup at W=8.
//   - Warm cache:    serial = N × ringSnapshot copy; fan-out = ⌈N/W⌉ ×
//     ringSnapshot copy; modest (~3×) speedup but tail-latency improves.
//
// Nil-safe: when h.scheduler is nil the caller short-circuits earlier; we
// also guard inside so a future caller can pass an empty jobs slice and
// receive nil without panic.
//
// R236-PERF-08 (#525).
func (h *CronHandlers) batchRecentRuns(jobs []cron.JobWithNextRun, n int) [][]cron.CronRunSummary {
	if len(jobs) == 0 || h.scheduler == nil {
		return nil
	}
	out := make([][]cron.CronRunSummary, len(jobs))
	// Tasks queue: each worker pulls the next index and fans out to the
	// scheduler. Channel-of-int keeps the work distribution self-balancing
	// — a slow per-job lookup (cold cache, slow disk) does not pin a
	// single worker on a single job while its peers idle.
	tasks := make(chan int, len(jobs))
	for i := range jobs {
		tasks <- i
	}
	close(tasks)
	workers := batchRecentRunsWorkers
	if workers > len(jobs) {
		workers = len(jobs)
	}
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for idx := range tasks {
				out[idx] = h.scheduler.RecentRuns(jobs[idx].Job.ID, n)
			}
		}()
	}
	wg.Wait()
	return out
}

// GET /api/cron — list all cron jobs (unscoped, admin view).
//
// `?compact=1` opt-in (R236-SEC-08 / #494) clips Prompt to the first 256
// UTF-8 bytes per job and stamps prompt_truncated=true. The default (no
// query param) still returns the full prompt to preserve byte-equal wire
// shape for any out-of-tree consumer pre-dating the compact mode.
// Dashboard.js poll path passes compact=1; the editor/drawer detail view
// fetches a single full job via the existing GET path with compact off.
func (h *CronHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	// R242-CR-3: gate per-IP before the scheduler/FS work so a stolen
	// dashboard token cannot enumerate the job list (with embedded
	// RecentRuns(5) per job) at unbounded rate. Mirrors runsLimiter
	// usage in handleRunsList. Nil-guarded for hand-built tests.
	if h.listLimiter != nil && !h.listLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron list rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		// R230B-CR-3: keep wire shape `{"jobs":[]}` byte-equal to the prior
		// map[string]any{"jobs": []any{}} fast path. Explicit empty slice
		// (not nil) so json.Marshal emits `[]` rather than `null`.
		writeJSON(w, cronListResp{Jobs: []cronJobView{}})
		return
	}

	// R236-SEC-08 (#494): compact mode is opt-in via ?compact=1. Anything
	// else (missing param, "0", "false", arbitrary strings) keeps the
	// legacy full-prompt behaviour so existing curl / IM consumers don't
	// silently lose data when this server upgrades.
	compact := r.URL.Query().Get("compact") == "1"

	jobs := h.scheduler.ListAllJobsWithNextRun()
	// R241-PERF-1: capture once outside the loop; each non-paused job called
	// time.Now() and h.scheduler.StartedAt() (an atomic load) independently,
	// yielding O(n) syscalls/atomics for an effectively-constant value.
	now := time.Now()
	startedAt := h.scheduler.StartedAt()

	// R236-PERF-08 (#525): pre-fetch RecentRuns for every job in parallel
	// so the 1 Hz dashboard poll's serial fan-out across N jobs does not
	// stall behind the per-job recentCacheEntry.mu acquire chain. The
	// previous shape called scheduler.RecentRuns inside the per-job loop
	// below, so under load N tabs × N jobs × per-job lock acquire serialised
	// the entire list response on the slowest jobLock / warmCache pass.
	// The fan-out runs at most batchRecentRunsWorkers goroutines so the
	// scheduler's runStore is not flooded with goroutines on a 200-job
	// install (sync.Map.Load contention dominates above ~16 readers); the
	// per-job result lands in a pre-sized slice keyed by job index so the
	// main loop below sees a deterministic order with no map allocation.
	recentByIdx := h.batchRecentRuns(jobs, recentRunsPerJob)

	views := make([]cronJobView, 0, len(jobs))
	for idx, entry := range jobs {
		j := entry.Job
		// R236-SEC-08 (#494): compact mode clips Prompt to 256 UTF-8 bytes
		// and flags prompt_truncated so the dashboard knows to refetch the
		// full body before opening the editor / drawer. Default (compact
		// off) keeps the legacy full-prompt shape for backwards compat —
		// the previous comment ("dashboard fuzzy-search depends on full
		// prompt") still applies to the legacy path; compact callers must
		// fall back to title/work_dir/schedule/id substring match (or
		// future server-side search) for the truncated subset.
		prompt := j.Prompt
		truncated := false
		if compact {
			prompt, truncated = truncatePromptUTF8(j.Prompt, compactPromptPrefixBytes)
		}
		v := cronJobView{
			ID:              j.ID,
			Schedule:        j.Schedule,
			Prompt:          prompt,
			PromptTruncated: truncated,
			Title:           j.Title,
			Platform:        j.Platform,
			ChatID:          j.ChatID,
			CreatedBy:       j.CreatedBy,
			CreatedAt:       j.CreatedAt.UnixMilli(),
			Paused:          j.Paused,
			WorkDir:         j.WorkDir,
			NotifyPlatform:  j.NotifyPlatform,
			NotifyChatID:    j.NotifyChatID,
			LastResult:      j.LastResult,
			LastError:       j.LastError,
			LastErrorClass:  string(j.LastErrorClass),
			Notify:          j.Notify,
			FreshContext:    j.FreshContext,
			Backend:         j.Backend,
		}
		if !j.LastRunAt.IsZero() {
			v.LastRunAt = j.LastRunAt.UnixMilli()
		}
		if !entry.NextRun.IsZero() {
			v.NextRun = entry.NextRun.UnixMilli()
		}
		// missed-schedule 检测：cron-v2-polish §3.3 Increment C。
		// 只对非 paused 的 job 判定——paused 的任务用户主动停了，错过
		// 是预期行为不应告警。R245-PERF-4 (#857): route through
		// missedScheduleVerdict so the cron expression Parse is memoised
		// across the 1 Hz dashboard poll cadence — N jobs × T tabs no
		// longer fans out to N×T regexp NFA builds per second.
		if !j.Paused {
			if missed, prevAt := h.missedScheduleVerdict(&j, now, startedAt); missed {
				v.Missed = true
				v.MissedSince = prevAt.UnixMilli()
			}
		}
		// CurrentRun & Stats — P0 cron-run-history。CurrentRun 只在 job 正
		// 在执行时返回；空 stats 也省略以减少线上 noise。
		if cur, ok := h.scheduler.CurrentRun(j.ID); ok {
			v.CurrentRun = &cronCurrentRunView{
				RunID:     cur.RunID,
				StartedAt: cur.StartedAt.UnixMilli(),
				Phase:     cur.Phase,
				Trigger:   string(cur.Trigger),
				SessionID: cur.SessionID,
			}
		}
		if c := j.RunCounters; c.Total > 0 {
			v.Stats = &cronRunCountersView{
				Total:     c.Total,
				Succeeded: c.Succeeded,
				Failed:    c.Failed,
				Skipped:   c.Skipped,
				TimedOut:  c.TimedOut,
				Canceled:  c.Canceled,
			}
		}
		// recent_runs: P1 — 5 条 newest-first 摘要给卡片 tooltip 用。
		// 上限 5 是 wire 大小的折中：list response 总大小 = jobs × ~2KB。
		// 详情页要更多用 GET /api/cron/runs.
		//
		// R250-PERF-19 (#1122): pre-extend rv to len(recent) and use index
		// assignment in place of append. Skips the per-iteration cap/len
		// bookkeeping the append builtin pays even when the backing array
		// is already pre-sized — a 1Hz × N-tab × 50-job poll churns enough
		// of these short slices that the saved bound checks add up.
		//
		// R236-PERF-08 (#525): the per-job RecentRuns lookup now lives in
		// the bounded-fan-out pass above (batchRecentRuns). Read from the
		// pre-fetched slice indexed by jobs[] position so this loop stays
		// pure projection — no per-iter scheduler call, no per-iter mutex
		// acquire — and the worst-case wall time is bounded by ⌈N/W⌉ × per-
		// job lock cost rather than N × per-job lock cost.
		if recent := recentByIdx[idx]; len(recent) > 0 {
			rv := make([]cronRunSummaryView, len(recent))
			for i, r := range recent {
				rv[i] = cronSummaryToView(r)
			}
			v.RecentRuns = rv
		}
		views = append(views, v)
	}

	loc := h.scheduler.Location()
	// R246-PERF-17: reuse the `now` captured before the job loop so the
	// timezone label is computed at the same moment as the missed-schedule
	// check above. Calling time.Now() a second time here also wasted a
	// syscall on every list request.
	name, offset := now.In(loc).Zone()
	locName := loc.String()
	tzLabel := formatTZOffset(locName, offset)

	// R230B-CR-3: named struct in place of map[string]any keeps the json
	// encoder on the cached reflect path (one-time alloc) and lets the wire
	// shape be grepped directly from the type definition.
	resp := cronListResp{
		Jobs:          views,
		Timezone:      locName,
		TimezoneLabel: tzLabel,
		TimezoneAbbr:  name,
	}
	if def := h.scheduler.NotifyDefault(); def.IsSet() {
		// Expose the configured default so the UI can render helpful copy
		// like "notifications go to feishu (oc_xxx)" instead of just a
		// blank toggle. chat_id is already considered semi-public (appears
		// in message metadata) so surfacing it here is not a leak.
		resp.NotifyDefault = &cronNotifyDefaultView{
			Platform: def.Platform,
			ChatID:   def.ChatID,
		}
	}
	writeJSON(w, resp)
}

// httpErrPersistFailed writes the standard 500 body for the "in-memory
// mutation succeeded but on-disk persist failed" case. The five cron
// write handlers (create / delete / pause / resume / update) all surface
// cron.ErrPersistFailed identically — same status, same wording with
// only the verb differing — so the literal had drifted across five
// copy-paste sites. Centralising the format keeps the wording in one
// place and stops a future copy from accidentally diverging the
// operator-visible string. R250-CR-20.
func httpErrPersistFailed(w http.ResponseWriter, op string) {
	http.Error(w, "job "+op+" but not persisted; please check server logs", http.StatusInternalServerError)
}

// POST /api/cron — create a new cron job from dashboard.
func (h *CronHandlers) handleCreate(w http.ResponseWriter, r *http.Request) {
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	var req struct {
		Schedule       string `json:"schedule"`
		Prompt         string `json:"prompt"`
		Title          string `json:"title,omitempty"`
		WorkDir        string `json:"work_dir,omitempty"`
		NotifyPlatform string `json:"notify_platform,omitempty"`
		NotifyChatID   string `json:"notify_chat_id,omitempty"`
		Notify         *bool  `json:"notify,omitempty"`
		FreshContext   bool   `json:"fresh_context,omitempty"`
		// Backend pins the CLI backend for this job ("" = router default).
		// Per docs/rfc/multi-backend.md §9 cron RPC contract. Validated
		// by validateCronBackend to match the send.go shape contract.
		Backend string `json:"backend,omitempty"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16) // 64 KB
	if err := decodeJSONBody(r, &req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Schedule == "" {
		http.Error(w, "schedule is required", http.StatusBadRequest)
		return
	}
	if err := validateCronTitle(req.Title); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Cap schedule length before handing to validateSchedule → robfig/cron
	// parser. MaxBytesReader caps the whole body at 64 KB, but within that
	// envelope a single 63 KB schedule field would still reach the parser
	// and force per-field regex work. Mirrors handlePreview (line 381).
	if len(req.Schedule) > maxCronScheduleBytesDashboard {
		http.Error(w, "schedule too long", http.StatusBadRequest)
		return
	}
	if err := validateCronScheduleChars(req.Schedule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateCronPrompt(req.Prompt); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateCronBackend(req.Backend); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate work_dir if provided: must be under allowedRoot. Matches the
	// 403 Forbidden used by /api/sessions/send so clients see a uniform
	// status code for boundary violations rather than ambiguous 400s.
	if req.WorkDir != "" {
		if err := validateCronWorkDir(req.WorkDir); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		validated, err := validateWorkspace(req.WorkDir, h.allowedRoot)
		if err != nil {
			status, msg := classifyWorkspaceErr(err)
			slog.Debug("cron work_dir validation failed", "err", err)
			http.Error(w, msg, status)
			return
		}
		req.WorkDir = validated
	}

	// Guard: notify=true without any target (neither per-job override nor
	// scheduler default) would silently swallow notifications. Reject it
	// at the edge so users see the problem immediately.
	//
	// R242-SEC-11: a per-job override is only "set" when BOTH platform and
	// chat_id are present — half-configured (one filled, one blank) used to
	// quietly fall through to NotifyDefault, hiding what is almost always a
	// dashboard form-fill mistake (typo'd ChatID, lost focus before saving
	// platform). Reject the half-set case explicitly with a distinct error
	// so the user can self-correct, instead of letting it land on cron job
	// disk and silently route notifications to the global fallback target.
	if req.NotifyPlatform != "" || req.NotifyChatID != "" {
		if req.NotifyPlatform == "" || req.NotifyChatID == "" {
			http.Error(w, "notify_platform and notify_chat_id must be set together", http.StatusBadRequest)
			return
		}
	}
	if req.Notify != nil && *req.Notify {
		perJobSet := req.NotifyPlatform != "" && req.NotifyChatID != ""
		if !perJobSet && !h.scheduler.NotifyDefault().IsSet() {
			http.Error(w, "notify=true but no target configured: set cron.notify_default in config or provide notify_platform/notify_chat_id", http.StatusBadRequest)
			return
		}
	}

	if err := validateNotifyTarget(req.NotifyPlatform, req.NotifyChatID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	job := &cron.Job{
		Schedule:       req.Schedule,
		Prompt:         req.Prompt,
		Title:          req.Title,
		Platform:       "dashboard",
		ChatID:         "global",
		CreatedBy:      "dashboard",
		WorkDir:        req.WorkDir,
		NotifyPlatform: req.NotifyPlatform,
		NotifyChatID:   req.NotifyChatID,
		Notify:         req.Notify,
		FreshContext:   req.FreshContext,
		Backend:        req.Backend,
		Paused:         req.Prompt == "", // auto-pause when no prompt
	}
	if err := h.scheduler.AddJob(job); err != nil {
		// ErrPersistFailed signals the job was inserted into the in-memory
		// map and cron scheduler but JSON marshal (and therefore the on-disk
		// store) failed; surface it as 500 so operators see the persistence
		// gap instead of the dashboard silently treating the create as a
		// successful 2xx that won't survive a restart. R51-QUAL-001.
		if errors.Is(err, cron.ErrPersistFailed) {
			slog.Error("cron AddJob persisted in-memory but store write failed", "err", err, "id", job.ID)
			httpErrPersistFailed(w, "created")
			return
		}
		// robfig/cron parser errors can mention internal field offsets and
		// parsed expressions; log the full detail for operator triage but
		// return a sanitized message to the dashboard client.
		slog.Warn("cron AddJob rejected", "err", err, "schedule", job.Schedule)
		http.Error(w, "invalid schedule or job fields", http.StatusBadRequest)
		return
	}

	slog.Info("cron job created via dashboard", "id", job.ID, "schedule", job.Schedule)
	writeJSON(w, cronCreateResp{ID: job.ID})
}

// DELETE /api/cron?id=xxx — delete a cron job by exact ID.
func (h *CronHandlers) handleDelete(w http.ResponseWriter, r *http.Request) {
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	// Reject obviously-oversized ids before reaching the scheduler so slog
	// attrs in the error path aren't dragged up to multi-MB strings.
	// maxCronIDLen (64) matches the IM-side guard in dispatch/commands.go.
	if len(id) > maxCronIDLenDashboard {
		http.Error(w, "id too long", http.StatusBadRequest)
		return
	}
	// [R250-SEC-1] Shape gate before id reaches scheduler/slog: keeps log
	// attributes free of newlines/control bytes that would inject forged
	// records into the operator log when the lookup misses.
	if !cron.IsValidID(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	j, err := h.scheduler.DeleteJobByID(id)
	if err != nil {
		switch {
		case errors.Is(err, cron.ErrJobNotFound):
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, cron.ErrPersistFailed):
			// In-memory + cron entry deletion already happened, but the
			// store write failed — a restart would replay the deleted job.
			// 500 alerts the operator to inspect logs instead of treating
			// the delete as quietly successful. R51-QUAL-001.
			slog.Error("cron DeleteJobByID deletion not persisted", "err", err, "id", id)
			httpErrPersistFailed(w, "deleted")
		default:
			slog.Debug("cron delete failed", "err", err)
			http.Error(w, "delete failed", http.StatusBadRequest)
		}
		return
	}

	slog.Info("cron job deleted via dashboard", "id", j.ID)
	writeOK(w)
}

// POST /api/cron/pause — pause a cron job by exact ID.
func (h *CronHandlers) handlePause(w http.ResponseWriter, r *http.Request) {
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KB
	if err := decodeJSONBody(r, &req); err != nil || req.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	// Mirror handleDelete's guard so oversized IDs don't drag slog attrs up
	// to KB-scale strings on failure/success paths. R64-SEC-1.
	if len(req.ID) > maxCronIDLenDashboard {
		http.Error(w, "id too long", http.StatusBadRequest)
		return
	}
	// [R250-SEC-1] Shape gate before id reaches scheduler/slog.
	if !cron.IsValidID(req.ID) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if _, err := h.scheduler.PauseJobByID(req.ID); err != nil {
		switch {
		case errors.Is(err, cron.ErrJobNotFound):
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, cron.ErrJobAlreadyPaused):
			http.Error(w, "job already paused", http.StatusConflict)
		case errors.Is(err, cron.ErrPersistFailed):
			slog.Error("cron PauseJobByID pause not persisted", "err", err, "id", req.ID)
			httpErrPersistFailed(w, "paused")
		default:
			slog.Debug("cron pause failed", "err", err)
			http.Error(w, "pause failed", http.StatusBadRequest)
		}
		return
	}

	slog.Info("cron job paused via dashboard", "id", req.ID)
	writeOK(w)
}

// POST /api/cron/resume — resume a paused cron job by exact ID.
func (h *CronHandlers) handleResume(w http.ResponseWriter, r *http.Request) {
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KB
	if err := decodeJSONBody(r, &req); err != nil || req.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	if len(req.ID) > maxCronIDLenDashboard {
		http.Error(w, "id too long", http.StatusBadRequest)
		return
	}
	// [R250-SEC-1] Shape gate before id reaches scheduler/slog.
	if !cron.IsValidID(req.ID) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if _, err := h.scheduler.ResumeJobByID(req.ID); err != nil {
		switch {
		case errors.Is(err, cron.ErrJobNotFound):
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, cron.ErrJobNotPaused):
			http.Error(w, "job not paused", http.StatusConflict)
		case errors.Is(err, cron.ErrPersistFailed):
			slog.Error("cron ResumeJobByID resume not persisted", "err", err, "id", req.ID)
			httpErrPersistFailed(w, "resumed")
		default:
			slog.Debug("cron resume failed", "err", err)
			http.Error(w, "resume failed", http.StatusBadRequest)
		}
		return
	}

	slog.Info("cron job resumed via dashboard", "id", req.ID)
	writeOK(w)
}

// POST /api/cron/trigger — manually trigger a cron job execution (for debugging).
func (h *CronHandlers) handleTrigger(w http.ResponseWriter, r *http.Request) {
	// [R247-SEC-2] Per-IP rate limit: each call spawns the cron job's
	// claude CLI subprocess and may emit IM notifications; without this
	// gate a stolen dashboard token could loop-trigger jobs to amplify
	// CPU/IM-quota damage. Nil-guarded for hand-built test handlers.
	if h.writeLimiter != nil && !h.writeLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron write rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	if err := decodeJSONBody(r, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	if len(req.ID) > maxCronIDLenDashboard {
		http.Error(w, "id too long", http.StatusBadRequest)
		return
	}
	// [R250-SEC-1] Shape gate before id reaches scheduler/slog.
	if !cron.IsValidID(req.ID) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.scheduler.TriggerNow(req.ID); err != nil {
		switch {
		case errors.Is(err, cron.ErrJobNotFound):
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, cron.ErrJobPaused):
			http.Error(w, "job is paused", http.StatusConflict)
		case errors.Is(err, cron.ErrJobNoPrompt):
			http.Error(w, "job has no prompt", http.StatusUnprocessableEntity)
		default:
			slog.Debug("cron trigger failed", "err", err)
			http.Error(w, "trigger failed", http.StatusBadRequest)
		}
		return
	}

	slog.Info("cron job triggered manually", "id", req.ID)
	writeJSON(w, map[string]string{"status": "triggered"})
}

// GET /api/cron/preview?schedule=...&count=N — validate schedule and return
// the next N run times. count defaults to 1 and is clamped to [1, 10] so the
// UI can show a multi-run preview without giving callers an unbounded knob.
func (h *CronHandlers) handlePreview(w http.ResponseWriter, r *http.Request) {
	// [R247-SEC-3] Per-IP rate limit. Although preview is read-only and
	// cheaper than trigger, it is not a heartbeat endpoint — each call
	// runs the cron parser plus N=1..10 next-run computations, so an
	// unbounded loop from a stolen token still burns CPU. Share the cron
	// write/control bucket to keep the per-IP control surface uniform.
	if h.writeLimiter != nil && !h.writeLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron write rate limit exceeded"})
		return
	}
	schedule := r.URL.Query().Get("schedule")
	if schedule == "" {
		http.Error(w, "schedule is required", http.StatusBadRequest)
		return
	}
	// Cap schedule length so the cron parser (regex + split) cannot be DoS'd
	// with a megabyte-scale query parameter. Real cron expressions are far
	// below this limit; robfig/cron rejects extremely long descriptors anyway.
	if len(schedule) > maxCronScheduleBytesDashboard {
		http.Error(w, "schedule too long", http.StatusBadRequest)
		return
	}
	if err := validateCronScheduleChars(schedule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	count := 1
	if raw := r.URL.Query().Get("count"); raw != "" {
		// Reject obviously huge inputs before Atoi so an attacker cannot force
		// us to decode a multi-kilobyte digit string.
		if len(raw) > 3 {
			http.Error(w, "count must be a positive integer", http.StatusBadRequest)
			return
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			http.Error(w, "count must be a positive integer", http.StatusBadRequest)
			return
		}
		if n > 10 {
			n = 10
		}
		count = n
	}

	// PreviewScheduleN / Location are nil-receiver-safe (R219-CR-6); the
	// nil path computes in UTC for tests / dashboard bootstrap before the
	// scheduler is wired, matching the behaviour of the deleted
	// cron.PreviewSchedule package-level helper.
	runs, err := h.scheduler.PreviewScheduleN(schedule, count)
	loc := h.scheduler.Location()
	tzName := loc.String()
	tzLabel := ""
	if n, offset := time.Now().In(loc).Zone(); n != "" {
		tzLabel = formatTZOffset(tzName, offset)
	}
	if err != nil {
		// Don't echo the raw robfig/cron parser error: it leaks field offsets
		// and internal token names that help an attacker enumerate accepted
		// grammar. Log the detail for operators instead.
		slog.Debug("cron preview parse failed", "err", err)
		writeJSON(w, cronPreviewResp{Valid: false, Error: "invalid schedule expression"})
		return
	}

	resp := cronPreviewResp{
		Valid:         true,
		Timezone:      tzName,
		TimezoneLabel: tzLabel, // omitempty drops the empty-zone case
	}
	if len(runs) > 0 {
		resp.NextRun = runs[0].UnixMilli()
		nextRuns := make([]int64, len(runs))
		for i, t := range runs {
			nextRuns[i] = t.UnixMilli()
		}
		resp.NextRuns = nextRuns
	}
	writeJSON(w, resp)
}

// PATCH /api/cron?id=xxx — edit schedule / prompt / work_dir on an existing job.
func (h *CronHandlers) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	if len(id) > maxCronIDLenDashboard {
		http.Error(w, "id too long", http.StatusBadRequest)
		return
	}
	// [R250-SEC-1] Shape gate before id reaches scheduler/slog.
	if !cron.IsValidID(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	// Use pointers so the caller can distinguish "leave as-is" from "clear".
	// Sending "work_dir": "" explicitly clears the override; omitting the key
	// leaves the existing value alone.
	var req struct {
		Schedule       *string `json:"schedule,omitempty"`
		Prompt         *string `json:"prompt,omitempty"`
		Title          *string `json:"title,omitempty"`
		WorkDir        *string `json:"work_dir,omitempty"`
		Notify         *bool   `json:"notify,omitempty"`
		NotifyPlatform *string `json:"notify_platform,omitempty"`
		NotifyChatID   *string `json:"notify_chat_id,omitempty"`
		FreshContext   *bool   `json:"fresh_context,omitempty"`
		// Backend pointer keeps "" semantics distinct from "leave alone":
		// nil omits, pointer-to-"" clears the override (router default),
		// pointer to a non-empty string sets it.
		Backend *string `json:"backend,omitempty"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := decodeJSONBody(r, &req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Schedule == nil && req.Prompt == nil && req.Title == nil && req.WorkDir == nil &&
		req.Notify == nil && req.NotifyPlatform == nil && req.NotifyChatID == nil &&
		req.FreshContext == nil && req.Backend == nil {
		http.Error(w, "at least one field must be provided", http.StatusBadRequest)
		return
	}
	if req.Prompt != nil {
		if err := validateCronPrompt(*req.Prompt); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if req.Title != nil {
		if err := validateCronTitle(*req.Title); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if req.Schedule != nil && len(*req.Schedule) > maxCronScheduleBytesDashboard {
		http.Error(w, "schedule too long", http.StatusBadRequest)
		return
	}
	if req.Schedule != nil {
		if err := validateCronScheduleChars(*req.Schedule); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if req.Backend != nil {
		if err := validateCronBackend(*req.Backend); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Re-validate workspace against allowedRoot; a cleared WorkDir is
	// accepted as-is and will fall back to the router default. 403 matches
	// handleCreate and the send handler for boundary violations.
	if req.WorkDir != nil && *req.WorkDir != "" {
		if err := validateCronWorkDir(*req.WorkDir); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		validated, err := validateWorkspace(*req.WorkDir, h.allowedRoot)
		if err != nil {
			status, msg := classifyWorkspaceErr(err)
			slog.Debug("cron work_dir validation failed", "err", err)
			http.Error(w, msg, status)
			return
		}
		req.WorkDir = &validated
	}

	// Guard: notify=true with no effective target would silently drop
	// notifications. Mirror the handleCreate check.
	if req.Notify != nil && *req.Notify {
		perJobSet := req.NotifyPlatform != nil && *req.NotifyPlatform != "" &&
			req.NotifyChatID != nil && *req.NotifyChatID != ""
		if !perJobSet && !h.scheduler.NotifyDefault().IsSet() {
			http.Error(w, "notify=true but no target configured: set cron.notify_default in config or provide notify_platform/notify_chat_id", http.StatusBadRequest)
			return
		}
	}

	// Validate notify target only when the caller is actually changing it.
	if req.NotifyPlatform != nil || req.NotifyChatID != nil {
		// R238-SEC-14: a PATCH that touches ONE notify field but omits the
		// other lands an orphan-target on disk. Concrete failure: the job
		// already has {platform="feishu", chat_id="oc_xxx"} and the caller
		// PATCHes notify_platform:"" without notify_chat_id — UpdateJob
		// clears NotifyPlatform but leaves NotifyChatID="oc_xxx", silently
		// re-routing notifications to the cron.notify_default fallback
		// instead of the explicit per-job target the operator just edited.
		// The platformSet/chatIDSet check below catches the (set,absent)
		// and (absent,set) cases but not (cleared-via-empty,absent) and
		// (absent,cleared-via-empty), because both halves coerce to "" and
		// the != check returns false. Force the caller to send both
		// pointers together so on-disk state always reflects a coherent
		// (both clear, both set) tuple. 422 mirrors the validation-shape
		// failure category — the request is well-formed JSON, the values
		// just describe an unprocessable on-disk transition.
		if (req.NotifyPlatform == nil) != (req.NotifyChatID == nil) {
			http.Error(w, "notify_platform and notify_chat_id must be patched together", http.StatusUnprocessableEntity)
			return
		}
		p := ""
		if req.NotifyPlatform != nil {
			p = *req.NotifyPlatform
		}
		c := ""
		if req.NotifyChatID != nil {
			c = *req.NotifyChatID
		}
		// R242-SEC-11: a half-set patch (one field present + non-empty,
		// the other present + empty OR absent) lands an orphan-target on
		// disk that silently routes notifications to the wrong place.
		// Disk shape we want is: both empty (no override) or both set.
		// Reject the half-set case so the caller can self-correct.
		// Patch leaves the missing pointer as nil — interpreted as
		// "leave existing", so a PATCH-of-one-field is a request to
		// edit one half: also disallowed for the same reason.
		platformSet := p != ""
		chatIDSet := c != ""
		if platformSet != chatIDSet {
			http.Error(w, "notify_platform and notify_chat_id must be set together", http.StatusBadRequest)
			return
		}
		if err := validateNotifyTarget(p, c); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	j, err := h.scheduler.UpdateJob(id, cron.JobUpdate{
		Schedule:       req.Schedule,
		Prompt:         req.Prompt,
		Title:          req.Title,
		WorkDir:        req.WorkDir,
		Notify:         req.Notify,
		NotifyPlatform: req.NotifyPlatform,
		NotifyChatID:   req.NotifyChatID,
		FreshContext:   req.FreshContext,
		Backend:        req.Backend,
	})
	if err != nil {
		switch {
		case errors.Is(err, cron.ErrJobNotFound):
			// Fixed string (not err.Error()) to stay consistent with
			// handleDelete and guard against future ErrJobNotFound variants
			// that carry a wrapped ID.
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, cron.ErrPersistFailed):
			slog.Error("cron UpdateJob update not persisted", "err", err, "id", id)
			httpErrPersistFailed(w, "updated")
		default:
			// Sanitize: the underlying parser error can leak internal field
			// names and offsets if the new schedule is rejected.
			slog.Warn("cron UpdateJob rejected", "err", err, "id", id)
			http.Error(w, "invalid update payload", http.StatusBadRequest)
		}
		return
	}

	slog.Info("cron job updated via dashboard", "id", j.ID)
	writeJSON(w, cronUpdateResp{Status: "ok", ID: j.ID})
}

// formatTZOffset renders a timezone label like "Asia/Shanghai (UTC+08:00)" or
// "America/St_Johns (UTC-03:30)". The ianaName parameter is the IANA zone
// identifier (loc.String()), NOT the abbr ("CST"/"NDT"). Both call sites
// surface ianaName here and the abbr in a separate timezone_abbr response
// field; the dashboard renders the IANA prefix because it's unambiguous
// across DST transitions, while abbr is shown alongside as a familiar
// short label. R230B-CR-2: parameter renamed from `name` to make the
// expected input explicit and prevent future call sites from passing the
// abbr by mistake.
//
// The integer-division approach would produce "UTC-05:-30" for fractional
// negative offsets because the sub-hour remainder inherits the sign;
// abs() the minute component to keep the format well-formed.
func formatTZOffset(ianaName string, offsetSeconds int) string {
	hours := offsetSeconds / 3600
	minutes := (offsetSeconds % 3600) / 60
	if minutes < 0 {
		minutes = -minutes
	}
	return fmt.Sprintf("%s (UTC%+03d:%02d)", ianaName, hours, minutes)
}

// runIDLenLimit caps the run_id query parameter length. Run IDs and job
// IDs share the same generator class (hex with headroom for future
// entropy bumps) and the same on-disk JSON store, so they share the
// cron.MaxIDLen constant. R230B-CR-1: previously a separate const was
// kept "in case of divergence", but no concrete plan exists for run IDs
// to grow longer than job IDs, and two parallel constants drifted in
// review with no source of truth. Reuse one constant; revisit if a real
// divergence requirement appears.
const runIDLenLimit = cron.MaxIDLen

// GET /api/cron/runs?job_id=&limit=&before=
//
// Returns CronRun summaries for one job, newest first. limit default 50,
// clamped to [1, cron.DefaultRunsKeepCount]. before is unix-ms; only runs
// strictly older than that timestamp are returned (paging cursor).
//
// Response shape:
//
//	{ "runs":[ { run_id, state, trigger, started_at, ended_at,
//	             duration_ms, session_id, error_class } ],
//	  "next_before": <unix-ms>   // omitted when no more pages
//	}
//
// Authenticated; no per-job ACL beyond the global dashboard auth gate
// (mirrors handleList's policy).
func (h *CronHandlers) handleRunsList(w http.ResponseWriter, r *http.Request) {
	// R222-SEC-3: gate per-IP before any scheduler / FS work so an attacker
	// holding a stolen dashboard token cannot enumerate the run history at
	// unbounded rate. Nil-guarded for hand-built tests.
	if h.runsLimiter != nil && !h.runsLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron runs rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		// R230B-CR-3: same byte shape as the map[string]any fast path —
		// `{"runs":[]}`. Explicit empty slice keeps json.Marshal off the
		// nil → "null" rendering branch.
		writeJSON(w, cronRunsListResp{Runs: []cronRunSummaryView{}})
		return
	}
	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		http.Error(w, "job_id is required", http.StatusBadRequest)
		return
	}
	if len(jobID) > maxCronIDLenDashboard {
		http.Error(w, "job_id too long", http.StatusBadRequest)
		return
	}
	if !cron.IsValidID(jobID) {
		http.Error(w, "job_id must be lowercase hex", http.StatusBadRequest)
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if len(raw) > 4 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		if n > cron.DefaultRunsKeepCount {
			n = cron.DefaultRunsKeepCount
		}
		limit = n
	}
	var before time.Time
	if raw := r.URL.Query().Get("before"); raw != "" {
		if len(raw) > 16 {
			http.Error(w, "before must be a unix-ms integer", http.StatusBadRequest)
			return
		}
		ms, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || ms <= 0 {
			http.Error(w, "before must be a unix-ms integer", http.StatusBadRequest)
			return
		}
		before = time.UnixMilli(ms)
	}

	rows := h.scheduler.ListRuns(jobID, limit, before)
	out := make([]cronRunSummaryView, 0, len(rows))
	for _, r := range rows {
		out = append(out, cronSummaryToView(r))
	}
	// R230B-CR-3: named struct in place of map[string]any keeps this 1Hz-poll
	// endpoint on the cached reflect path (one-time alloc) instead of paying
	// the per-call map iteration + interface boxing each request.
	resp := cronRunsListResp{Runs: out}
	// next_before: emit only when this page was full (caller may have more).
	// Conservative: a partial page can still indicate "no more" because runs
	// older than this batch may have been GC'd; we let the dashboard treat
	// next_before as "fetch older than this" hint.
	if len(out) == limit && len(out) > 0 {
		resp.NextBefore = out[len(out)-1].StartedAt
	}
	writeJSON(w, resp)
}

// GET /api/cron/runs/{run_id}?job_id=...
//
// Returns the full CronRun (Prompt + Result + ErrorMsg). 404 when missing,
// 500 with "corrupt record" message when the file exists but fails to
// parse / exceeds size cap. Used by the dashboard detail drawer.
func (h *CronHandlers) handleRunDetail(w http.ResponseWriter, r *http.Request) {
	// R222-SEC-3: same per-IP gate as handleRunsList. Detail reads also do
	// FS I/O (read JSON file from disk) so they share the limiter and
	// budget, not separate buckets — a single bucket prevents bypass-via-
	// alternate-endpoint when both URLs share an identical token.
	if h.runsLimiter != nil && !h.runsLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron runs rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}
	// Path param: /api/cron/runs/{run_id}; PathValue is supplied by the
	// http.ServeMux pattern at registration time. Defensive in case the
	// pattern is changed without updating this handler.
	runID := r.PathValue("run_id")
	if runID == "" {
		http.Error(w, "run_id is required", http.StatusBadRequest)
		return
	}
	if len(runID) > runIDLenLimit {
		http.Error(w, "run_id too long", http.StatusBadRequest)
		return
	}
	if !cron.IsValidID(runID) {
		http.Error(w, "run_id must be lowercase hex", http.StatusBadRequest)
		return
	}
	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		http.Error(w, "job_id is required", http.StatusBadRequest)
		return
	}
	if len(jobID) > maxCronIDLenDashboard {
		http.Error(w, "job_id too long", http.StatusBadRequest)
		return
	}
	if !cron.IsValidID(jobID) {
		http.Error(w, "job_id must be lowercase hex", http.StatusBadRequest)
		return
	}
	run, err := h.scheduler.GetRun(jobID, runID)
	if err != nil {
		if errors.Is(err, cron.ErrCorruptRun) {
			slog.Warn("cron run record corrupt", "job_id", jobID, "run_id", runID, "err", err)
			http.Error(w, "run record corrupt", http.StatusInternalServerError)
			return
		}
		// Default: treat any non-corrupt error (incl. fs.ErrNotExist)
		// as "not found" — distinguishing "not exist" vs "perm denied"
		// would leak filesystem layout to a remote caller.
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	// SanitizeForLog the Prompt + WorkDir fields read off disk: dashboard
	// validate* gates already strip control / bidi characters at the write
	// edge, but a CronRun persisted before the policy was tightened (or
	// hand-edited on disk) can carry runes that would render dangerously
	// in the dashboard. Result/ErrorMsg are already sanitised inside
	// recordResultP0WithSanitised before persistence.
	out := cronRunDetailView{
		RunID:       run.RunID,
		JobID:       run.JobID,
		State:       string(run.State),
		Trigger:     string(run.Trigger),
		StartedAt:   run.StartedAt.UnixMilli(),
		DurationMS:  run.DurationMS,
		SessionID:   run.SessionID,
		Prompt:      osutil.SanitizeForLog(run.Prompt, cron.MaxPromptBytes),
		WorkDir:     osutil.SanitizeForLog(run.WorkDir, maxCronWorkDirBytesDashboard),
		Fresh:       run.Fresh,
		Result:      run.Result,
		ResultBytes: run.ResultBytes,
		ErrorClass:  string(run.ErrorClass),
		ErrorMsg:    run.ErrorMsg,
	}
	if !run.EndedAt.IsZero() {
		out.EndedAt = run.EndedAt.UnixMilli()
	}
	writeJSON(w, out)
}
