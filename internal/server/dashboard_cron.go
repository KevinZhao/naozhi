package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
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
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != 0x7f {
			continue
		}
		if c == '\t' && p.allowTab {
			continue
		}
		if c == '\n' && p.allowLF {
			continue
		}
		if p.collapseErrors {
			return fmt.Errorf("%s contains invalid characters", p.name)
		}
		return fmt.Errorf("%s contains invalid control characters", p.name)
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
// 不接入 R219-CR-5 抽出的 validateStringField 是因为它把单行约束（\n/\r 即
// 报"title must be a single line"，与"contains invalid control characters"
// 含义不同）和 rune-级长度计量 (utf8.RuneCountInString) 一起做，stringFieldPolicy
// 当前只覆盖 Tab/LF allow-list 与 byte-级长度，不覆盖单行专用错误消息分支；
// 强行接入会反向把 4 个 cron 验证器都污染成"如果支持单行就额外提示"的样板。
func validateCronTitle(title string) error {
	if title == "" {
		return nil
	}
	if n := utf8.RuneCountInString(title); n > cron.MaxCronTitleLen {
		return fmt.Errorf("title exceeds %d-rune limit", cron.MaxCronTitleLen)
	}
	if !utf8.ValidString(title) {
		return fmt.Errorf("title contains invalid characters")
	}
	for _, r := range title {
		if r == '\n' || r == '\r' {
			return fmt.Errorf("title must be a single line")
		}
		if r == 0 || (r < 0x20 && r != '\t') || r == 0x7f {
			return fmt.Errorf("title contains invalid control characters")
		}
		if osutil.IsLogInjectionRune(r) {
			return fmt.Errorf("title contains invalid unicode control characters")
		}
	}
	return nil
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
}

// GET /api/cron — list all cron jobs (unscoped, admin view).
func (h *CronHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	if h.scheduler == nil {
		// R230B-CR-3: keep wire shape `{"jobs":[]}` byte-equal to the prior
		// map[string]any{"jobs": []any{}} fast path. Explicit empty slice
		// (not nil) so json.Marshal emits `[]` rather than `null`.
		writeJSON(w, cronListResp{Jobs: []cronJobView{}})
		return
	}

	jobs := h.scheduler.ListAllJobsWithNextRun()
	// R241-PERF-1: capture once outside the loop; each non-paused job called
	// time.Now() and h.scheduler.StartedAt() (an atomic load) independently,
	// yielding O(n) syscalls/atomics for an effectively-constant value.
	now := time.Now()
	startedAt := h.scheduler.StartedAt()
	views := make([]cronJobView, 0, len(jobs))
	for _, entry := range jobs {
		j := entry.Job
		// Prompt 不截断：dashboard.js 客户端 fuzzy-search 依赖完整 prompt
		// 内容（filterCronJobs 在 j.prompt 上做 substring match）。截断后
		// 搜索结果会假阴。8 KiB × 50 job = 400 KiB/响应 在 1 Hz 拉取下
		// 是已知开销，待后续移到 server-side search 后再优化。
		v := cronJobView{
			ID:             j.ID,
			Schedule:       j.Schedule,
			Prompt:         j.Prompt,
			Title:          j.Title,
			Platform:       j.Platform,
			ChatID:         j.ChatID,
			CreatedBy:      j.CreatedBy,
			CreatedAt:      j.CreatedAt.UnixMilli(),
			Paused:         j.Paused,
			WorkDir:        j.WorkDir,
			NotifyPlatform: j.NotifyPlatform,
			NotifyChatID:   j.NotifyChatID,
			LastResult:     j.LastResult,
			LastError:      j.LastError,
			LastErrorClass: string(j.LastErrorClass),
			Notify:         j.Notify,
			FreshContext:   j.FreshContext,
			Backend:        j.Backend,
		}
		if !j.LastRunAt.IsZero() {
			v.LastRunAt = j.LastRunAt.UnixMilli()
		}
		if !entry.NextRun.IsZero() {
			v.NextRun = entry.NextRun.UnixMilli()
		}
		// missed-schedule 检测：cron-v2-polish §3.3 Increment C。
		// 只对非 paused 的 job 判定——paused 的任务用户主动停了，错过
		// 是预期行为不应告警。
		if !j.Paused {
			if missed, prevAt := cron.HasMissedSchedule(&j, now, startedAt); missed {
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
		if recent := h.scheduler.RecentRuns(j.ID, 5); len(recent) > 0 {
			rv := make([]cronRunSummaryView, 0, len(recent))
			for _, r := range recent {
				rv = append(rv, cronSummaryToView(r))
			}
			v.RecentRuns = rv
		}
		views = append(views, v)
	}

	loc := h.scheduler.Location()
	name, offset := time.Now().In(loc).Zone()
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
			http.Error(w, "job created but not persisted; please check server logs", http.StatusInternalServerError)
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
	writeJSON(w, map[string]any{"id": job.ID})
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
			http.Error(w, "job deleted but not persisted; please check server logs", http.StatusInternalServerError)
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

	if _, err := h.scheduler.PauseJobByID(req.ID); err != nil {
		switch {
		case errors.Is(err, cron.ErrJobNotFound):
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, cron.ErrJobAlreadyPaused):
			http.Error(w, "job already paused", http.StatusConflict)
		case errors.Is(err, cron.ErrPersistFailed):
			slog.Error("cron PauseJobByID pause not persisted", "err", err, "id", req.ID)
			http.Error(w, "job paused but not persisted; please check server logs", http.StatusInternalServerError)
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

	if _, err := h.scheduler.ResumeJobByID(req.ID); err != nil {
		switch {
		case errors.Is(err, cron.ErrJobNotFound):
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, cron.ErrJobNotPaused):
			http.Error(w, "job not paused", http.StatusConflict)
		case errors.Is(err, cron.ErrPersistFailed):
			slog.Error("cron ResumeJobByID resume not persisted", "err", err, "id", req.ID)
			http.Error(w, "job resumed but not persisted; please check server logs", http.StatusInternalServerError)
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
		writeJSON(w, map[string]any{"valid": false, "error": "invalid schedule expression"})
		return
	}

	resp := map[string]any{
		"valid":    true,
		"timezone": tzName,
	}
	if tzLabel != "" {
		resp["timezone_label"] = tzLabel
	}
	if len(runs) > 0 {
		resp["next_run"] = runs[0].UnixMilli()
		nextRuns := make([]int64, len(runs))
		for i, t := range runs {
			nextRuns[i] = t.UnixMilli()
		}
		resp["next_runs"] = nextRuns
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
		p := ""
		if req.NotifyPlatform != nil {
			p = *req.NotifyPlatform
		}
		c := ""
		if req.NotifyChatID != nil {
			c = *req.NotifyChatID
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
			http.Error(w, "job updated but not persisted; please check server logs", http.StatusInternalServerError)
		default:
			// Sanitize: the underlying parser error can leak internal field
			// names and offsets if the new schedule is rejected.
			slog.Warn("cron UpdateJob rejected", "err", err, "id", id)
			http.Error(w, "invalid update payload", http.StatusBadRequest)
		}
		return
	}

	slog.Info("cron job updated via dashboard", "id", j.ID)
	writeJSON(w, map[string]any{"status": "ok", "id": j.ID})
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
	type runDetailView struct {
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
	// SanitizeForLog the Prompt + WorkDir fields read off disk: dashboard
	// validate* gates already strip control / bidi characters at the write
	// edge, but a CronRun persisted before the policy was tightened (or
	// hand-edited on disk) can carry runes that would render dangerously
	// in the dashboard. Result/ErrorMsg are already sanitised inside
	// recordResultP0WithSanitised before persistence.
	out := runDetailView{
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
