package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// trimUnicodeSpace strips all Unicode whitespace (including full-width
// ideographic space U+3000, NBSP, zero-width space) from both ends of s.
// Plain strings.TrimSpace only handles ASCII + \t\n\v\f\r, so CJK users
// who pressed space on a Chinese IME see their /cd path / /project arg
// silently fall through to the "unknown command" branch.
func trimUnicodeSpace(s string) string {
	return strings.TrimFunc(s, unicode.IsSpace)
}

// replyText sends a text reply to msg.ChatID via the matching platform, logging
// but not returning errors. Resolves d.platforms[msg.Platform] internally and
// is a no-op if that platform is not registered. Returns true if the reply was
// attempted (regardless of success), false if the platform was unknown — this
// lets callers short-circuit follow-up logic that only makes sense when a user
// actually receives feedback.
func (d *Dispatcher) replyText(ctx context.Context, msg platform.IncomingMessage, text string, log *slog.Logger) bool {
	p := d.platforms[msg.Platform]
	if p == nil {
		return false
	}
	if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: text}); err != nil {
		if log != nil {
			log.Warn("reply failed", "err", err)
		} else {
			slog.Warn("reply failed", "platform", msg.Platform, "chat", msg.ChatID, "err", err)
		}
	}
	return true
}

// normalizeSlashCommand lowercases the leading "/command" token only, leaving
// arguments untouched. CJK mobile IMEs commonly auto-capitalize the first
// letter of a line (e.g. "/New foo") which would otherwise fall through to
// the unknown-command branch. Trailing whitespace is stripped so IMEs that
// append a space before Enter do not break the bare "/help" equality check.
func normalizeSlashCommand(trimmed string) string {
	if !strings.HasPrefix(trimmed, "/") {
		return trimmed
	}
	sp := strings.IndexByte(trimmed, ' ')
	if sp < 0 {
		// No ASCII space but the command may still carry trailing unicode
		// whitespace (e.g. U+3000 IDEOGRAPHIC SPACE from a CJK IME). Without
		// TrimRight those bare commands would fail the `trimmed == "/help"`
		// equality check and fall through to the unknown-command branch.
		return strings.TrimRightFunc(strings.ToLower(trimmed), unicode.IsSpace)
	}
	return strings.TrimRightFunc(strings.ToLower(trimmed[:sp])+trimmed[sp:], unicode.IsSpace)
}

// dispatchCommand handles slash commands (/help, /new, /clear, /cron, /cd, /pwd, /project).
// Returns true if the message was a command and was handled.
func (d *Dispatcher) dispatchCommand(ctx context.Context, msg platform.IncomingMessage, trimmed string, log *slog.Logger) bool {
	trimmed = normalizeSlashCommand(trimmed)
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
		if d.projectMgr != nil {
			if proj := d.projectMgr.ProjectForChat(msg.Platform, msg.ChatType, msg.ChatID); proj != nil {
				d.replyText(ctx, msg, fmt.Sprintf("当前已绑定项目 %s，工作目录固定为项目路径。如需切换，请先 /project off 解绑。", proj.Name), log)
				return true
			}
		}
		d.handleCdCommand(ctx, msg, trimmed, log)
		return true

	case trimmed == "/pwd":
		chatKey := session.ChatKey(msg.Platform, msg.ChatType, msg.ChatID)
		ws := d.router.GetWorkspace(chatKey)
		d.replyText(ctx, msg, "当前工作目录: "+ws, log)
		return true

	case trimmed == "/project" || strings.HasPrefix(trimmed, "/project "):
		d.handleProjectCommand(ctx, msg, trimmed, log)
		return true

	case trimmed == "/new" || strings.HasPrefix(trimmed, "/new ") ||
		trimmed == "/clear" || strings.HasPrefix(trimmed, "/clear "):
		d.handleNewCommand(ctx, msg, trimmed, log)
		return true

	default:
		return false
	}
}

func (d *Dispatcher) handleHelpCommand(ctx context.Context, msg platform.IncomingMessage) {
	help := "可用命令:\n" +
		"  /help — 显示此帮助\n" +
		"  /new [agent] — 重置会话\n" +
		"  /clear — 重置会话（同 /new）\n" +
		"  /cd <路径> — 切换工作目录\n" +
		"  /pwd — 显示当前工作目录\n" +
		"  /project [name|off|list] — 项目绑定\n" +
		"  /cron <add|list|del|pause|resume> — 定时任务"
	if len(d.agentCommands) > 0 {
		help += "\n\n可用 Agent:"
		for cmd, agentID := range d.agentCommands {
			help += "\n  /" + cmd + " → " + agentID
		}
	}
	d.replyText(ctx, msg, help, nil)
}

func (d *Dispatcher) handleNewCommand(ctx context.Context, msg platform.IncomingMessage, trimmed string, log *slog.Logger) {
	agentToReset := ""
	if parts := strings.SplitN(trimmed, " ", 2); len(parts) > 1 {
		// agentCommands keys are pre-normalized to lowercase in applyDefaults;
		// match the user-supplied agent name case-insensitively so "/new REVIEW"
		// still resolves.
		agentToReset = strings.ToLower(trimUnicodeSpace(parts[1]))
	}

	// In project-bound mode: /new resets planner, /new {agent} resets that agent
	if d.projectMgr != nil {
		if proj := d.projectMgr.ProjectForChat(msg.Platform, msg.ChatType, msg.ChatID); proj != nil {
			if agentToReset == "" {
				plannerKey := proj.PlannerSessionKey()
				d.router.Reset(plannerKey)
				d.discardQueue(plannerKey)
				d.replyText(ctx, msg, "项目 "+proj.Name+" 的 planner 已重置。", log)
			} else {
				if id, ok := d.agentCommands[agentToReset]; ok {
					key := session.SessionKey(msg.Platform, msg.ChatType, msg.ChatID, id)
					d.router.Reset(key)
					d.discardQueue(key)
					d.replyText(ctx, msg, "会话已重置 ("+id+")。", log)
				} else {
					d.replyText(ctx, msg, "未知的 agent: "+agentToReset, log)
				}
			}
			return
		}
	}

	agentID := "general"
	if agentToReset != "" {
		if id, ok := d.agentCommands[agentToReset]; ok {
			agentID = id
		} else {
			found := false
			for _, id := range d.agentCommands {
				if id == agentToReset {
					agentID = id
					found = true
					break
				}
			}
			if !found {
				errMsg := "未知的 agent: " + agentToReset
				if len(d.agentCommands) > 0 {
					var names []string
					for cmd := range d.agentCommands {
						names = append(names, cmd)
					}
					errMsg += "\n可用: " + strings.Join(names, ", ")
				}
				d.replyText(ctx, msg, errMsg, log)
				return
			}
		}
	}
	key := session.SessionKey(msg.Platform, msg.ChatType, msg.ChatID, agentID)
	d.router.Reset(key)
	d.discardQueue(key)
	label := ""
	if agentID != "general" {
		label = " (" + agentID + ")"
	}
	d.replyText(ctx, msg, "对话已重置"+label+"。", log)
	log.Info("session reset by user", "agent", agentID)
}

// handleCronCommand dispatches /cron subcommands (add, list, del, pause, resume).
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
		if len(parts) < 3 {
			reply("用法: /cron add \"<schedule>\" <prompt>\n例: /cron add \"@every 30m\" 检查服务状态")
			return
		}
		schedule, prompt, err := ParseCronAdd(parts[2])
		if err != nil {
			reply("格式错误: " + err.Error() + "\n用法: /cron add \"<schedule>\" <prompt>")
			return
		}
		job := &cron.Job{
			Schedule:  schedule,
			Prompt:    prompt,
			Platform:  msg.Platform,
			ChatID:    msg.ChatID,
			ChatType:  msg.ChatType,
			CreatedBy: msg.UserID,
		}
		if err := d.scheduler.AddJob(job); err != nil {
			// AddJob wraps the raw schedule string + robfig/cron parser
			// internals into the error; echoing that to IM leaks both the
			// server-normalized form of the attacker's input and parser
			// token positions. Log the detail for operator triage, reply
			// with a generic message. Mirrors dashboard_cron handleCreate.
			log.Warn("cron AddJob rejected", "err", err, "schedule", job.Schedule)
			reply("创建失败：请检查定时表达式格式")
			return
		}
		next := d.scheduler.NextRun(job)
		reply(fmt.Sprintf("Job %s 已创建。Schedule: %s, Next: %s", job.ID, job.Schedule, next.Format("01/02 15:04")))
		log.Info("cron job created", "id", job.ID, "schedule", job.Schedule)

	case "list":
		jobs := d.scheduler.ListJobs(msg.Platform, msg.ChatID)
		if len(jobs) == 0 {
			reply("当前聊天没有定时任务。")
			return
		}
		var sb strings.Builder
		sb.WriteString("定时任务:\n")
		for _, j := range jobs {
			status := ""
			if j.Paused {
				status = " [暂停]"
			}
			fmt.Fprintf(&sb, "  %s  %-20s %s%s\n", j.ID, j.Schedule, j.Prompt, status)
		}
		reply(sb.String())

	case "del":
		if len(parts) < 3 {
			reply("用法: /cron del <id>")
			return
		}
		if len(parts[2]) > maxCronIDLen {
			reply("无效 ID")
			return
		}
		j, err := d.scheduler.DeleteJob(parts[2], msg.Platform, msg.ChatID)
		if err != nil {
			// Echoing err.Error() to IM leaks internal scheduler state
			// (normalized ID form, lock annotations). Dashboard already
			// sanitises analogous handlers. Log raw, reply generic.
			log.Warn("cron DeleteJob failed", "err", err, "id_prefix", parts[2])
			reply("删除失败：请确认 ID 正确")
			return
		}
		reply(fmt.Sprintf("Job %s 已删除。", j.ID))
		log.Info("cron job deleted", "id", j.ID)

	case "pause":
		if len(parts) < 3 {
			reply("用法: /cron pause <id>")
			return
		}
		if len(parts[2]) > maxCronIDLen {
			reply("无效 ID")
			return
		}
		j, err := d.scheduler.PauseJob(parts[2], msg.Platform, msg.ChatID)
		if err != nil {
			log.Warn("cron PauseJob failed", "err", err, "id_prefix", parts[2])
			reply("暂停失败：请确认 ID 正确或任务是否已暂停")
			return
		}
		reply(fmt.Sprintf("Job %s 已暂停。", j.ID))
		log.Info("cron job paused", "id", j.ID)

	case "resume":
		if len(parts) < 3 {
			reply("用法: /cron resume <id>")
			return
		}
		if len(parts[2]) > maxCronIDLen {
			reply("无效 ID")
			return
		}
		j, err := d.scheduler.ResumeJob(parts[2], msg.Platform, msg.ChatID)
		if err != nil {
			log.Warn("cron ResumeJob failed", "err", err, "id_prefix", parts[2])
			reply("恢复失败：请确认 ID 正确或任务是否已暂停")
			return
		}
		next := d.scheduler.NextRun(j)
		reply(fmt.Sprintf("Job %s 已恢复。Next: %s", j.ID, next.Format("01/02 15:04")))
		log.Info("cron job resumed", "id", j.ID)

	default:
		reply("用法: /cron <add|list|del|pause|resume>\n" +
			"  /cron add \"@every 30m\" 检查服务状态\n" +
			"  /cron add \"0 9 * * 1-5\" /review 扫描 open PRs\n" +
			"  /cron list\n" +
			"  /cron del <id>\n" +
			"  /cron pause <id>\n" +
			"  /cron resume <id>")
	}
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
		proj := d.projectMgr.Get(arg)
		if proj == nil {
			d.replyText(ctx, msg, "项目不存在: "+arg+"\n使用 /project list 查看可用项目。", log)
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

	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, path[1:])
		}
	}

	var absPath string
	if filepath.IsAbs(path) {
		absPath = filepath.Clean(path)
	} else {
		chatKey := session.ChatKey(msg.Platform, msg.ChatType, msg.ChatID)
		currentWS := d.router.GetWorkspace(chatKey)
		absPath = filepath.Join(currentWS, path)
	}

	// Resolve symlinks BEFORE Stat + allowedRoot check so a swap between
	// Stat and EvalSymlinks cannot hand us different filesystem entries —
	// same ordering as server.validateWorkspace, closes a TOCTOU window
	// where a symlink re-target between the two calls would let a user
	// cd outside allowedRoot.
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

	if d.allowedRoot != "" && absPath != d.allowedRoot && !strings.HasPrefix(absPath, d.allowedRoot+string(filepath.Separator)) {
		d.replyText(ctx, msg, "不允许访问该路径", log)
		return
	}

	chatKey := session.ChatKey(msg.Platform, msg.ChatType, msg.ChatID)
	d.router.SetWorkspace(chatKey, absPath)
	d.router.ResetChat(chatKey)

	d.replyText(ctx, msg, "工作目录已切换到: "+absPath+"\n所有会话已重置，新消息将在此目录下执行。", log)
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

// maxCronPromptBytes bounds the prompt body accepted via `/cron add` so a single
// IM message can't stuff megabytes into cron_jobs.json. The limit mirrors the
// dashboard planner_prompt cap — anything beyond this is almost certainly a
// cut-paste mistake, and every cron run replays the full prompt through the
// CLI stdin, so runaway sizes multiply across invocations.
const maxCronPromptBytes = 8 * 1024

// maxCronIDLen bounds the ID accepted from IM `/cron del|pause|resume <id>`
// commands. Generated IDs are 8-char hex (see scheduler.generateID); 64 bytes
// leaves slack for future ID schemes while preventing multi-MB inputs from
// propagating into log/error allocations on the miss path.
const maxCronIDLen = 64

// maxCronScheduleBytes caps the schedule expression length. robfig/cron
// expressions are short (e.g. "@every 30m", "0 9 * * *"); anything beyond
// 256 bytes is almost certainly abuse. Matches the dashboard preview guard.
const maxCronScheduleBytes = 256

// ParseCronAdd parses the args of /cron add: "schedule" prompt
func ParseCronAdd(args string) (schedule, prompt string, err error) {
	args = smartQuoteNormalizer.Replace(args)
	if !strings.HasPrefix(args, "\"") {
		return "", "", fmt.Errorf("schedule must be quoted, e.g. \"@every 30m\"")
	}
	// strings.Cut handles the "" closing quote search + tail separation as a
	// single operation, avoiding manual byte arithmetic that could surprise
	// on non-ASCII schedule text (e.g. someone embedding Chinese in a desc).
	rest, tail, ok := strings.Cut(args[1:], "\"")
	if !ok {
		return "", "", fmt.Errorf("missing closing quote for schedule")
	}
	schedule = rest
	// Bound schedule length before handing to the parser: robfig/cron splits
	// on whitespace and runs regex per field, so a multi-KB schedule would
	// force measurable parser work even though it's guaranteed to fail. The
	// dashboard preview handler enforces the same 256-byte cap.
	if len(schedule) > maxCronScheduleBytes {
		return "", "", fmt.Errorf("schedule too long (max %d bytes)", maxCronScheduleBytes)
	}
	// Control chars in schedule would persist verbatim into cron_jobs.json
	// and could corrupt NDJSON framing when the job's prompt replays through
	// shim stdin. Printable + space + tab is sufficient for every valid cron
	// expression the robfig/cron parser accepts.
	for i := 0; i < len(schedule); i++ {
		c := schedule[i]
		if c == 0 || (c < 0x20 && c != '\t') || c == 0x7f {
			return "", "", fmt.Errorf("schedule contains invalid control characters")
		}
	}
	prompt = strings.TrimSpace(tail)
	if prompt == "" {
		return "", "", fmt.Errorf("prompt cannot be empty")
	}
	if len(prompt) > maxCronPromptBytes {
		return "", "", fmt.Errorf("prompt too long (max %d bytes)", maxCronPromptBytes)
	}
	// Reject the same control-character set the dashboard rejects: null bytes
	// are silently truncated by execve and raw \r/\n into --append-system-prompt
	// corrupts shim NDJSON framing. Tab is allowed because prompts may indent
	// examples. Mirrors server.validateCronPrompt (dashboard_cron.go).
	for i := 0; i < len(prompt); i++ {
		c := prompt[i]
		if c == 0 || (c < 0x20 && c != '\t') || c == 0x7f {
			return "", "", fmt.Errorf("prompt contains invalid control characters")
		}
	}
	return schedule, prompt, nil
}
