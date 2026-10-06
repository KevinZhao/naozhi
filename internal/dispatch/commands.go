package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session/sessionview"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/textutil"
	"github.com/naozhi/naozhi/internal/turn"
)

// trimUnicodeSpace strips all Unicode whitespace (U+3000 ideographic space,
// NBSP, zero-width space, ...) from both ends of s so CJK IME input doesn't
// fall through to the "unknown command" branch.
func trimUnicodeSpace(s string) string {
	return strings.TrimFunc(s, unicode.IsSpace)
}

// replyText sends a text reply to msg's chat and thread via the matching platform,
// logging but not returning errors. Returns false (no-op) when the platform
// is unregistered so callers can skip follow-up logic.
func (d *Dispatcher) replyText(ctx context.Context, msg platform.IncomingMessage, text string, log *slog.Logger) bool {
	p := d.platforms[msg.Platform]
	if p == nil {
		return false
	}
	if _, err := p.Reply(ctx, replyDestOf(msg).text(text)); err != nil {
		if log != nil {
			log.Warn("reply failed", "err", err)
		} else {
			// nil log: msg.ChatID/Platform come straight from webhook payloads,
			// so sanitize like BuildHandler's enriched logger does.
			slog.Warn("reply failed",
				"platform", sessionkey.SanitizeLogAttr(msg.Platform),
				"chat", sessionkey.SanitizeLogAttr(msg.ChatID),
				"err", err)
		}
	}
	return true
}

// dispatchCommand handles slash commands (/help, /new, /clear, /urgent, /cron,
// /cd, /pwd, /project, /stop). Returns true if the message was a command and
// was handled. turn.Parse recognises the turn commands; the rest match its
// normalized Text. A switch rather than a handler table: arms carry unique
// preconditions (/cd consults the project binding, /cron needs a scheduler).
func (d *Dispatcher) dispatchCommand(ctx context.Context, msg platform.IncomingMessage, trimmed string, log *slog.Logger) bool {
	cmd := turn.Parse(trimmed)
	switch cmd.Kind {
	case turn.CmdReset:
		d.handleNewCommand(ctx, msg, cmd.Arg, log)
		return true
	case turn.CmdUrgent:
		d.handleUrgentCommand(ctx, msg, cmd.Arg, log)
		return true
	case turn.CmdUrgentUsage:
		d.replyText(ctx, msg, "用法：/urgent <紧急消息>（该消息会立即中断正在进行的回复）", log)
		return true
	}
	trimmed = cmd.Text
	switch {
	case trimmed == "/cron" || strings.HasPrefix(trimmed, "/cron "):
		if d.scheduler != nil {
			d.handleCronCommand(ctx, msg, trimmed, log)
		}
		return true

	case trimmed == "/help":
		d.handleHelpCommand(ctx, msg)
		return true

	case strings.HasPrefix(trimmed, "/cd "):
		// Read project state through the resolver so /cd sees the same snapshot
		// the IM hot path's key derivation used (#648).
		if b := d.resolver.ProjectBindingForChat(msg.Platform, msg.ChatType, msg.ChatID); b.Bound {
			d.replyText(ctx, msg, fmt.Sprintf("当前已绑定项目 %s，工作目录固定为项目路径。如需切换，请先 /project off 解绑。", b.Name), log)
			return true
		}
		d.handleCdCommand(ctx, msg, trimmed, log)
		return true

	case trimmed == "/pwd":
		chatKey := sessionkey.ChatKey(msg.Platform, msg.ChatType, msg.ChatID)
		ws := d.router.Workspace(chatKey)
		if ws == "" {
			d.replyText(ctx, msg, "当前工作目录: （未设置，使用进程默认）", log)
			return true
		}
		// Defence-in-depth: operator-supplied state dirs may carry control chars.
		d.replyText(ctx, msg, "当前工作目录: "+osutil.SanitizeForLog(ws, 4096), log)
		return true

	case trimmed == "/project" || strings.HasPrefix(trimmed, "/project "):
		d.handleProjectCommand(ctx, msg, trimmed, log)
		return true

	case isStopCommand(trimmed):
		d.handleStopCommand(ctx, msg, log)
		return true

	default:
		return false
	}
}

// handleStopCommand aborts the in-flight turn for this chat's session via the
// CLI's control_request interrupt (ACP sessions fall back to Interrupt()). In
// passthrough mode pending slots stay queued — only the active turn drops. The
// interrupt is broadcast across every agent the chat could have a live session
// for, so /stop also works for agent-command turns (#1944).
func (d *Dispatcher) handleStopCommand(ctx context.Context, msg platform.IncomingMessage, log *slog.Logger) {
	outcome := d.interruptChat(msg.Platform, msg.ChatType, msg.ChatID)
	switch outcome {
	case sessionview.InterruptSent:
		d.replyText(ctx, msg, "已中断当前回复。", log)
	case sessionview.InterruptNoTurn, sessionview.InterruptNoSession:
		d.replyText(ctx, msg, "当前没有正在进行的回复。", log)
	case sessionview.InterruptUnsupported:
		d.replyText(ctx, msg, "当前后端不支持软中断。", log)
	case sessionview.InterruptError:
		d.replyText(ctx, msg, "中断失败，请稍后重试或使用 /new 重置会话。", log)
	}
}

// interruptChat broadcasts a control_request interrupt across every agent
// session a chat may own (general/planner + every agent-command target) and
// folds the outcomes best-news-wins, so an idle planner never masks an
// actually-interrupted code-reviewer turn:
//
//	Sent > Error > Unsupported > NoTurn > NoSession
//
// Keys are deduplicated: one agentID may be reachable via several commands.
func (d *Dispatcher) interruptChat(platform, chatType, chatID string) sessionview.InterruptOutcome {
	agentIDs := make(map[string]struct{}, len(d.agentCommands)+2)
	agentIDs["general"] = struct{}{}
	agentIDs["planner"] = struct{}{}
	for _, id := range d.agentCommands {
		agentIDs[id] = struct{}{}
	}

	best := sessionview.InterruptNoSession
	rank := func(o sessionview.InterruptOutcome) int {
		switch o {
		case sessionview.InterruptSent:
			return 4
		case sessionview.InterruptError:
			return 3
		case sessionview.InterruptUnsupported:
			return 2
		case sessionview.InterruptNoTurn:
			return 1
		default: // InterruptNoSession
			return 0
		}
	}

	seenKey := make(map[string]struct{}, len(agentIDs))
	for id := range agentIDs {
		key := d.keyForChat(platform, chatType, chatID, id)
		if _, dup := seenKey[key]; dup {
			continue
		}
		seenKey[key] = struct{}{}
		if o := d.router.InterruptSessionViaControl(key); rank(o) > rank(best) {
			best = o
		}
	}
	return best
}

// handleUrgentCommand submits text at turn.PriorityNow: the CLI aborts any
// in-flight turn and runs the urgent message next; messages already queued
// stay queued and get their own answers afterwards. Protocols without
// passthrough (ACP) fall back to InterruptViaControl + Send. turn.Parse never
// yields an empty text.
func (d *Dispatcher) handleUrgentCommand(ctx context.Context, msg platform.IncomingMessage, text string, log *slog.Logger) {
	// Resolve via KeyResolver so /urgent gets the same project-bound opts as
	// the main IM path (docs/rfc/key-resolver.md §2.1 #3).
	agentID := "general"
	key, opts := d.resolver.ResolveForChat(msg.Platform, msg.ChatType, msg.ChatID, agentID)
	o := d.newIMOrigin(msg, log, key, agentID, opts, imUrgent, len(text), 0)
	d.submit(ctx, o, turn.Request{Key: key, Text: text, Priority: turn.PriorityNow})
}

func (d *Dispatcher) handleHelpCommand(ctx context.Context, msg platform.IncomingMessage) {
	help := "可用命令:\n" +
		"  /help — 显示此帮助\n" +
		"  /new [agent] — 重置会话\n" +
		"  /clear — 重置会话（同 /new）\n" +
		"  /stop — 中断当前回复（保留后续排队消息）\n" +
		"  /urgent <消息> — 紧急打断并优先处理该消息\n" +
		"  /cd <路径> — 切换工作目录\n" +
		"  /pwd — 显示当前工作目录\n" +
		"  /project [name|off|list] — 项目绑定\n" +
		"  /cron <add|list|del|pause|resume|mode> — 定时任务"
	if len(d.agentCommands) > 0 {
		help += "\n\n可用 Agent:"
		// Sort so /help output is stable (map iteration order is random).
		cmds := make([]string, 0, len(d.agentCommands))
		for cmd := range d.agentCommands {
			cmds = append(cmds, cmd)
		}
		slices.Sort(cmds)
		for _, cmd := range cmds {
			agentID := d.agentCommands[cmd]
			// Sanitize operator-configured names before forwarding to group chats.
			help += "\n  /" + osutil.SanitizeForLog(cmd, 64) + " → " + osutil.SanitizeForLog(agentID, 64)
		}
	}
	d.replyText(ctx, msg, help, nil)
}

// resolveAgentToken maps a user-supplied (lowercased) /new <agent> token to a
// stored agent ID: exact lookup against agentCommands keys (pre-lowercased in
// applyDefaults), then an EqualFold scan over values so mixed-case operator
// IDs like "ReviewerBot" still resolve. Shared by both handleNewCommand
// branches so the fallback can't drift between them.
func (d *Dispatcher) resolveAgentToken(agentToReset string) (string, bool) {
	if id, ok := d.agentCommands[agentToReset]; ok {
		return id, true
	}
	for _, id := range d.agentCommands {
		if strings.EqualFold(id, agentToReset) {
			return id, true
		}
	}
	return "", false
}

// handleNewCommand resets the chat's session for /new and /clear; arg is the
// optional agent token. The reset goes through Turns.Reset, which discards
// the key's queue (clearing the dropped messages' ⏳) before the session
// reset retires the key (#2185), and keeps the /cd workspace override.
func (d *Dispatcher) handleNewCommand(ctx context.Context, msg platform.IncomingMessage, arg string, log *slog.Logger) {
	// agentCommands keys are lowercased in applyDefaults; match case-insensitively.
	agentToReset := strings.ToLower(trimUnicodeSpace(arg))

	// Project-bound chat: /new resets planner, /new {agent} resets that agent.
	// Read the binding through the resolver so /new and the IM hot path see
	// the same snapshot (#648).
	if b := d.resolver.ProjectBindingForChat(msg.Platform, msg.ChatType, msg.ChatID); b.Bound {
		if agentToReset == "" {
			plannerKey := d.keyForChat(msg.Platform, msg.ChatType, msg.ChatID, "general")
			d.turns.Reset(ctx, plannerKey, false)
			d.replyText(ctx, msg, "项目 "+b.Name+" 的 planner 已重置。", log)
		} else {
			if id, ok := d.resolveAgentToken(agentToReset); ok {
				key := d.keyForChat(msg.Platform, msg.ChatType, msg.ChatID, id)
				d.turns.Reset(ctx, key, false)
				d.replyText(ctx, msg, "会话已重置 ("+id+")。", log)
			} else {
				// User input: sanitize before echoing into chat (bidi spoofing).
				d.replyText(ctx, msg, "未知的 agent: "+osutil.SanitizeForLog(agentToReset, 64), log)
			}
		}
		return
	}

	agentID := "general"
	if agentToReset != "" {
		if id, ok := d.resolveAgentToken(agentToReset); ok {
			agentID = id
		} else {
			// User input: sanitize before echoing back (bidi spoofing).
			errMsg := "未知的 agent: " + osutil.SanitizeForLog(agentToReset, 64)
			if len(d.agentCommands) > 0 {
				// Sort so the hint line is stable.
				names := make([]string, 0, len(d.agentCommands))
				for cmd := range d.agentCommands {
					names = append(names, cmd)
				}
				slices.Sort(names)
				errMsg += "\n可用: " + strings.Join(names, ", ")
			}
			d.replyText(ctx, msg, errMsg, log)
			return
		}
	}
	key := sessionkey.SessionKey(msg.Platform, msg.ChatType, msg.ChatID, agentID)
	d.turns.Reset(ctx, key, false)
	label := ""
	if agentID != "general" {
		label = " (" + agentID + ")"
	}
	d.replyText(ctx, msg, "对话已重置"+label+"。", log)
	log.Info("session reset by user", "agent", agentID)
}

// handleCronCommand dispatches /cron subcommands (add, list, del, pause, resume, mode).
func (d *Dispatcher) handleCronCommand(ctx context.Context, msg platform.IncomingMessage, trimmed string, log *slog.Logger) {
	if d.platforms[msg.Platform] == nil {
		return
	}
	reply := func(text string) { d.replyText(ctx, msg, text, log) }

	parts := strings.SplitN(trimmed, " ", 3)
	sub := ""
	if len(parts) >= 2 {
		// Sub-commands are case-insensitive to cover IME auto-capitalization
		// (e.g. "/cron ADD …"). IDs in parts[2] stay case-sensitive.
		sub = strings.ToLower(parts[1])
	}

	switch sub {
	case "add":
		d.handleCronAdd(msg, parts, reply, log)
	case "list":
		d.handleCronList(msg, reply)
	case "del":
		d.handleCronDel(msg, parts, reply, log)
	case "pause":
		d.handleCronPause(msg, parts, reply, log)
	case "resume":
		d.handleCronResume(msg, parts, reply, log)
	case "mode":
		d.handleCronMode(msg, parts, reply, log)
	default:
		reply("用法: /cron <add|list|del|pause|resume|mode>\n" +
			"  /cron add \"@every 30m\" 检查服务状态\n" +
			"  /cron add \"0 9 * * 1-5\" /review 扫描 open PRs\n" +
			"  /cron add --keep-context \"0 18 * * *\" 接着昨天的进度写日报\n" +
			"  /cron list\n" +
			"  /cron del <id>\n" +
			"  /cron pause <id>\n" +
			"  /cron resume <id>\n" +
			"  /cron mode <id> fresh|keep")
	}
}

// cronAddUsage is the /cron add synopsis shown on a malformed invocation.
const cronAddUsage = "用法: /cron add [--keep-context] \"<schedule>\" <prompt>"

// handleCronAdd implements /cron add [--keep-context] "<schedule>" <prompt>.
// IM jobs start every run from a fresh session unless --keep-context is
// given, because a kept context grows by one turn per run; /cron mode
// switches it later.
func (d *Dispatcher) handleCronAdd(msg platform.IncomingMessage, parts []string, reply func(string), log *slog.Logger) {
	if len(parts) < 3 {
		reply(cronAddUsage + "\n例: /cron add \"@every 30m\" 检查服务状态")
		return
	}
	args, err := ParseCronAdd(parts[2])
	if err != nil {
		reply("格式错误: " + err.Error() + "\n" + cronAddUsage)
		return
	}
	schedule := args.Schedule
	job, next, err := d.scheduler.AddJob(CronJobRequest{
		Schedule:     schedule,
		Prompt:       args.Prompt,
		Platform:     msg.Platform,
		ChatID:       msg.ChatID,
		ChatType:     msg.ChatType,
		CreatedBy:    msg.UserID,
		FreshContext: !args.KeepContext,
	})
	if err != nil {
		// Never echo err.Error(): it leaks the normalized schedule and parser
		// internals. Log (sanitized — ParseCronAdd only gates ASCII C0/DEL) for
		// operators, reply by error class.
		log.Warn("cron AddJob rejected", "err", err,
			"schedule", osutil.SanitizeForLog(schedule, 256))
		reply(cronAddErrReply(d.scheduler.ClassifyError(err)))
		return
	}
	// Defence-in-depth against future parser relaxations.
	reply(fmt.Sprintf("Job %s 已创建。Schedule: %s, Next: %s\n%s",
		job.ID,
		osutil.SanitizeForLog(job.Schedule, 256),
		formatCronNext(next),
		cronContextNote(job.ID, job.FreshContext)))
	log.Info("cron job created", "id", job.ID,
		"schedule", osutil.SanitizeForLog(job.Schedule, 256),
		"fresh_context", job.FreshContext)
}

// cronContextNote tells the creator which context mode job id runs in and
// the /cron mode command that switches to the other one.
func cronContextNote(id string, fresh bool) string {
	if fresh {
		return "每次执行都从新会话开始；需要延续上次的上下文，发送 /cron mode " + id + " keep"
	}
	return "每次执行延续同一会话的上下文；需要每次从新会话开始，发送 /cron mode " + id + " fresh"
}

// cronAddErrReply maps a /cron add failure's wire code (from
// CronCommands.ClassifyError) to its user-facing reply, so a full quota or a
// too-frequent schedule is not reported as a malformed expression. The
// interval floor mirrors cron's minCronInterval.
func cronAddErrReply(code string) string {
	switch code {
	case CronCodeInvalidPrompt:
		return "创建失败：任务内容不合法（为空、过长或含控制字符）"
	case CronCodeJobQuotaExceeded:
		return "创建失败：定时任务数已达上限，请先用 /cron del 删除不用的任务"
	case CronCodeIntervalTooShort:
		return "创建失败：执行间隔不能短于 5 分钟"
	case CronCodeInvalidSchedule:
		return "创建失败：请检查定时表达式格式"
	default:
		return "创建失败，请稍后再试"
	}
}

// formatCronNext renders a next-run time with year and zone abbreviation.
// The time arrives in the scheduler's cron.timezone, which need not be the
// reader's own zone. A zero time (no live entry) renders as "—".
func formatCronNext(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("2006-01-02 15:04 MST")
}

// sanitizeCronDisplay prepares a cron job field (Schedule or Prompt) for IM
// display: collapses \n/\t, truncates to maxRunes, strips C0/C1/bidi and
// escapes markdown link punctuation to prevent link-smuggling.
func sanitizeCronDisplay(s string, maxRunes int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	// Truncate by runes so CJK characters don't cause byte-count overrun.
	runes := []rune(s)
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
		s = string(runes) + "…"
	} else {
		s = string(runes)
	}
	s = osutil.SanitizeForLog(s, len(s)*4+16)
	// Prevent markdown link-smuggling via [text](url) (#1707).
	s = textutil.EscapeCronMarkdownPunct(s)
	return s
}

// handleCronList implements /cron list.
func (d *Dispatcher) handleCronList(msg platform.IncomingMessage, reply func(string)) {
	jobs := d.scheduler.ListJobs(msg.Platform, msg.ChatID)
	if len(jobs) == 0 {
		reply("当前聊天没有定时任务。")
		return
	}
	var sb strings.Builder
	sb.WriteString("定时任务:\n")
	for _, j := range jobs {
		status := ""
		if !j.FreshContext {
			status = " [保留上下文]"
		}
		if j.AutoPaused {
			status += " [自动暂停：连续失败]"
		} else if j.Paused {
			status += " [暂停]"
		}
		safeSchedule := sanitizeCronDisplay(j.Schedule, 30)
		safePrompt := sanitizeCronDisplay(j.Prompt, 30)
		fmt.Fprintf(&sb, "  %s  %-20s %s%s\n", j.ID, safeSchedule, safePrompt, status)
	}
	reply(sb.String())
}

// cronMutationErrReply maps a /cron del|pause|resume|mode failure to a specific
// user-facing reply so an ambiguous prefix or a pause/resume state conflict
// is distinguishable from a bad ID. code is the wire code from
// CronCommands.ClassifyError (#1164); raw err.Error() is never echoed (it
// leaks normalized ID form / lock annotations). verb is the Chinese action
// label used in the generic fallback.
func cronMutationErrReply(verb, code string) string {
	switch code {
	case CronCodeAmbiguousPrefix:
		return "ID 前缀匹配到多个任务，请输入更长的 ID 以消歧。"
	case CronCodeJobAlreadyPaused:
		return "该任务已处于暂停状态。"
	case CronCodeJobNotPaused:
		return "该任务未处于暂停状态，无需恢复。"
	case CronCodeJobNotFound:
		return verb + "失败：未找到该 ID 对应的任务，请确认 ID 正确。"
	default:
		return verb + "失败：请确认 ID 正确。"
	}
}

// handleCronDel implements /cron del <id>.
func (d *Dispatcher) handleCronDel(msg platform.IncomingMessage, parts []string, reply func(string), log *slog.Logger) {
	if !validateCronIDArg(parts, "del", reply) {
		return
	}
	j, err := d.scheduler.DeleteJob(parts[2], msg.Platform, msg.ChatID)
	if err != nil {
		// Never echo err.Error() (leaks scheduler internals); log raw, reply generic.
		log.Warn("cron DeleteJob failed", "err", err, "id_prefix", parts[2])
		reply(cronMutationErrReply("删除", d.scheduler.ClassifyError(err)))
		return
	}
	reply(fmt.Sprintf("Job %s 已删除。", j.ID))
	log.Info("cron job deleted", "id", j.ID)
}

// handleCronPause implements /cron pause <id>.
func (d *Dispatcher) handleCronPause(msg platform.IncomingMessage, parts []string, reply func(string), log *slog.Logger) {
	if !validateCronIDArg(parts, "pause", reply) {
		return
	}
	j, err := d.scheduler.PauseJob(parts[2], msg.Platform, msg.ChatID)
	if err != nil {
		log.Warn("cron PauseJob failed", "err", err, "id_prefix", parts[2])
		reply(cronMutationErrReply("暂停", d.scheduler.ClassifyError(err)))
		return
	}
	reply(fmt.Sprintf("Job %s 已暂停。", j.ID))
	log.Info("cron job paused", "id", j.ID)
}

// handleCronResume implements /cron resume <id>.
func (d *Dispatcher) handleCronResume(msg platform.IncomingMessage, parts []string, reply func(string), log *slog.Logger) {
	if !validateCronIDArg(parts, "resume", reply) {
		return
	}
	j, next, err := d.scheduler.ResumeJob(parts[2], msg.Platform, msg.ChatID)
	if err != nil {
		log.Warn("cron ResumeJob failed", "err", err, "id_prefix", parts[2])
		reply(cronMutationErrReply("恢复", d.scheduler.ClassifyError(err)))
		return
	}
	reply(fmt.Sprintf("Job %s 已恢复。Next: %s", j.ID, formatCronNext(next)))
	log.Info("cron job resumed", "id", j.ID)
}

// cronModeUsage is the /cron mode synopsis shown on a malformed invocation.
const cronModeUsage = "用法: /cron mode <id> fresh|keep"

// handleCronMode implements /cron mode <id> fresh|keep: whether the job's runs
// start from a fresh session, from its next run on. The mode word is
// case-insensitive and keep-context is an alias of keep, as in /cron add.
func (d *Dispatcher) handleCronMode(msg platform.IncomingMessage, parts []string, reply func(string), log *slog.Logger) {
	var args []string
	if len(parts) == 3 {
		args = strings.Fields(parts[2])
	}
	if len(args) != 2 {
		reply(cronModeUsage)
		return
	}
	id := args[0]
	if len(id) > maxCronIDLen {
		reply("无效 ID")
		return
	}
	var fresh bool
	switch strings.ToLower(args[1]) {
	case "fresh":
		fresh = true
	case "keep", "keep-context":
	default:
		reply(cronModeUsage)
		return
	}
	j, err := d.scheduler.SetFreshContext(id, msg.Platform, msg.ChatID, fresh)
	if err != nil {
		log.Warn("cron SetFreshContext failed", "err", err, "id_prefix", id)
		reply(cronMutationErrReply("修改", d.scheduler.ClassifyError(err)))
		return
	}
	mode := "延续同一会话的上下文"
	if j.FreshContext {
		mode = "都从新会话开始"
	}
	text := fmt.Sprintf("Job %s 已改为每次执行%s（下次执行生效）。", j.ID, mode)
	if j.Paused {
		text += "\n该任务当前已暂停，发送 /cron resume " + j.ID + " 恢复。"
	}
	reply(text)
	log.Info("cron job context mode set", "id", j.ID, "fresh_context", j.FreshContext)
}

// validateCronIDArg checks parts has a third token (the job ID) within
// maxCronIDLen; replies usage / "无效 ID" and returns false otherwise.
func validateCronIDArg(parts []string, sub string, reply func(string)) bool {
	if len(parts) < 3 {
		reply("用法: /cron " + sub + " <id>")
		return false
	}
	if len(parts[2]) > maxCronIDLen {
		reply("无效 ID")
		return false
	}
	return true
}

// handleProjectCommand handles /project [name|off|list] commands.
func (d *Dispatcher) handleProjectCommand(ctx context.Context, msg platform.IncomingMessage, trimmed string, log *slog.Logger) {
	if d.platforms[msg.Platform] == nil {
		return
	}
	if d.projectMgr == nil {
		d.replyText(ctx, msg, "项目功能未启用（未配置 projects.root）。", log)
		return
	}

	arg := trimUnicodeSpace(strings.TrimPrefix(trimmed, "/project"))
	// Reserved keywords are case-insensitive; project names remain
	// case-sensitive (handled by the default branch).
	switch strings.ToLower(arg) {
	case "":
		proj := d.projectMgr.ProjectForChat(msg.Platform, msg.ChatType, msg.ChatID)
		if proj == nil {
			d.replyText(ctx, msg, "当前未绑定项目。\n用法: /project <项目名> 绑定", log)
		} else {
			d.replyText(ctx, msg, fmt.Sprintf("当前绑定: %s (%s)", proj.Name, proj.Path), log)
		}

	case "off":
		if err := d.projectMgr.UnbindAllChat(msg.Platform, msg.ChatType, msg.ChatID); err != nil {
			log.Warn("project unbind failed", "err", err)
			d.replyText(ctx, msg, "解绑失败，请稍后重试。", log)
			return
		}
		d.replyText(ctx, msg, "已解绑项目，恢复默认路由。", log)
		log.Info("project unbound", "chat", msg.ChatID)

	case "list":
		projects := d.projectMgr.All()
		if len(projects) == 0 {
			d.replyText(ctx, msg, "无可用项目。", log)
			return
		}
		var lines []string
		for _, proj := range projects {
			lines = append(lines, fmt.Sprintf("  %s — %s", proj.Name, proj.Path))
		}
		d.replyText(ctx, msg, "可用项目:\n"+strings.Join(lines, "\n"), log)

	default:
		// Validate at the IM trust boundary so a crafted name cannot inject
		// into slog attrs or be echoed back; same gate as dashboard/reverse-RPC.
		if err := project.ValidateProjectName(arg); err != nil {
			d.replyText(ctx, msg, "项目名不合法。\n使用 /project list 查看可用项目。", log)
			return
		}
		proj := d.projectMgr.Get(arg)
		if proj == nil {
			// Never echo the user-supplied name back into a group chat.
			d.replyText(ctx, msg, "项目不存在。\n使用 /project list 查看可用项目。", log)
			return
		}
		if err := d.projectMgr.BindChat(proj.Name, msg.Platform, msg.ChatType, msg.ChatID); err != nil {
			log.Warn("project bind failed", "project", proj.Name, "err", err)
			d.replyText(ctx, msg, "绑定失败，请稍后重试。", log)
			return
		}
		d.replyText(ctx, msg, fmt.Sprintf("已绑定项目: %s\n后续消息将路由到该项目的 planner。", proj.Name), log)
		log.Info("project bound", "project", proj.Name, "chat", msg.ChatID)
	}
}

// handleCdCommand changes the working directory for all sessions in a chat.
func (d *Dispatcher) handleCdCommand(ctx context.Context, msg platform.IncomingMessage, trimmed string, log *slog.Logger) {
	if d.platforms[msg.Platform] == nil {
		return
	}

	path := trimUnicodeSpace(strings.TrimPrefix(trimmed, "/cd"))
	if path == "" {
		d.replyText(ctx, msg, "用法: /cd <目录路径>\n例: /cd /home/ubuntu/my-project", log)
		return
	}

	// Echo the user-typed form later: tilde expansion would leak the server's
	// /home/<user> prefix into the IM channel.
	originalInput := path

	if strings.HasPrefix(path, "~") {
		// Fail fast: leaving "~/foo" unexpanded would hit the relative-path
		// branch and yield a misleading "目录不存在或无权限".
		home, err := os.UserHomeDir()
		if err != nil {
			d.replyText(ctx, msg, "无法获取用户主目录，请使用绝对路径（例: /cd /home/ubuntu/project）", log)
			return
		}
		path = filepath.Join(home, path[1:])
	}

	chatKey := sessionkey.ChatKey(msg.Platform, msg.ChatType, msg.ChatID)

	var absPath string
	if filepath.IsAbs(path) {
		absPath = filepath.Clean(path)
	} else {
		currentWS := d.router.Workspace(chatKey)
		// A relative path needs a prior /cd <abs> anchor; otherwise it would
		// resolve against the process cwd, meaningless to the user.
		if currentWS == "" {
			d.replyText(ctx, msg, "请先用绝对路径设置工作目录（例: /cd /home/ubuntu/project）再使用相对路径", log)
			return
		}
		absPath = filepath.Join(currentWS, path)
	}

	// Resolve symlinks BEFORE Stat + allowedRoot check (same order as
	// server.validateWorkspace) so a symlink re-target between the calls
	// cannot let a user cd outside allowedRoot.
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		d.replyText(ctx, msg, "目录不存在或无权限", log)
		return
	}
	absPath = resolved

	info, err := os.Stat(absPath)
	if err != nil || !info.IsDir() {
		d.replyText(ctx, msg, "目录不存在或无权限", log)
		return
	}

	if d.allowedRoot != "" {
		// Canonicalise allowedRoot the same way as absPath; on EvalSymlinks
		// failure fall back to the raw string (matches server.validateWorkspace
		// / cron.workDirResolveUnderRoot). PathContainedInRoot byte-compares
		// with an inode-walk fallback for case-insensitive filesystems.
		rootResolved := d.allowedRoot
		if r, err := filepath.EvalSymlinks(d.allowedRoot); err == nil {
			rootResolved = r
		}
		if !osutil.PathContainedInRoot(absPath, rootResolved) {
			d.replyText(ctx, msg, "不允许访问该路径", log)
			return
		}
	}

	// Atomic reset+set under one Router lock (#2342): a separate ResetChat +
	// SetWorkspace pair lets a concurrent GetOrCreate spawn a session in the
	// old workspace before the new path is installed.
	d.router.ResetChatAndSetWorkspace(chatKey, absPath)

	// Echo the pre-expansion input, never absPath (symlink targets / home dir).
	displayPath := filepath.Clean(originalInput)
	// Scrub bidi overrides / control chars before echoing into chat.
	displayPath = osutil.SanitizeForLog(displayPath, 4096)
	d.replyText(ctx, msg, "工作目录已切换到: "+displayPath+"\n所有会话已重置，新消息将在此目录下执行。", log)
	log.Info("workspace changed", "chat_key", chatKey, "path", absPath)
}

// smartQuoteNormalizer maps typographic / CJK quote glyphs to the plain ASCII
// double-quote so users composing messages on iOS/macOS (which auto-replace
// ASCII `"` with `“”`) or CJK keyboards (which default to 「」) can still use
// the /cron add "schedule" prompt syntax without fighting autocorrect.
var smartQuoteNormalizer = strings.NewReplacer(
	"\u201c", "\"", // LEFT DOUBLE QUOTATION MARK “
	"\u201d", "\"", // RIGHT DOUBLE QUOTATION MARK ”
	"\u300c", "\"", // LEFT CORNER BRACKET 「
	"\u300d", "\"", // RIGHT CORNER BRACKET 」
	"\u2018", "\"", // LEFT SINGLE QUOTATION MARK ‘ — treat as doublequote too
	"\u2019", "\"", // RIGHT SINGLE QUOTATION MARK ’
)

// maxCronIDLen guards the `/cron <op> <id>` token; aliased from the leaf
// textutil package so this edge doesn't import the cron domain package (#1707).
const maxCronIDLen = textutil.MaxCronIDLen

// CronAddArgs is the parsed form of /cron add's arguments.
type CronAddArgs struct {
	Schedule string
	Prompt   string
	// KeepContext is set by a leading --keep-context (or --keep).
	KeepContext bool
}

// ParseCronAdd parses the args of /cron add: [--keep-context] "schedule" prompt
func ParseCronAdd(args string) (CronAddArgs, error) {
	args = smartQuoteNormalizer.Replace(args)
	keep, args, err := cutCronAddFlag(args)
	if err != nil {
		return CronAddArgs{}, err
	}
	if !strings.HasPrefix(args, "\"") {
		return CronAddArgs{}, fmt.Errorf("schedule must be quoted, e.g. \"@every 30m\"")
	}
	schedule, tail, ok := strings.Cut(args[1:], "\"")
	if !ok {
		return CronAddArgs{}, fmt.Errorf("missing closing quote for schedule")
	}
	// Shared with the dashboard edge so the two policies cannot drift (#1315).
	if err := textutil.ValidateCronScheduleChars(schedule); err != nil {
		return CronAddArgs{}, err
	}
	// Char screening alone passes an all-whitespace schedule; reject it clearly.
	if strings.TrimSpace(schedule) == "" {
		return CronAddArgs{}, fmt.Errorf("定时表达式不能为空")
	}
	prompt := strings.TrimSpace(tail)
	// Same helper as dashboard validateCronPrompt / Scheduler.SetJobPrompt so
	// IM and dashboard ingress never diverge (#1315). LF/Tab stay allowed for
	// multi-line playbooks; CR is not rejected here unlike the dashboard.
	if err := textutil.ValidateCronPromptStrict(prompt); err != nil {
		return CronAddArgs{}, err
	}
	return CronAddArgs{Schedule: schedule, Prompt: prompt, KeepContext: keep}, nil
}

// cutCronAddFlag strips an optional option token in front of the quoted
// schedule. Phone keyboards turn "--" into an en or em dash, CJK IMEs into
// a full-width hyphen, and may capitalize the word, so any dash run and any
// letter case is accepted. A schedule is always quoted, so any other
// dash-led token is an unknown option.
func cutCronAddFlag(args string) (keep bool, rest string, err error) {
	args = strings.TrimLeftFunc(args, unicode.IsSpace)
	name := strings.TrimLeft(args, "-\u2013\u2014\u2212\uff0d")
	if len(name) == len(args) {
		return false, args, nil
	}
	end := strings.IndexFunc(name, func(r rune) bool { return r == '"' || unicode.IsSpace(r) })
	if end < 0 {
		end = len(name)
	}
	if !strings.EqualFold(name[:end], "keep-context") && !strings.EqualFold(name[:end], "keep") {
		return false, "", fmt.Errorf("未知选项（仅支持 --keep-context）")
	}
	return true, strings.TrimLeftFunc(name[end:], unicode.IsSpace), nil
}
