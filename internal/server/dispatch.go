package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/routing"
	"github.com/naozhi/naozhi/internal/session"
)

func (s *Server) buildMessageHandler() platform.MessageHandler {
	return func(ctx context.Context, msg platform.IncomingMessage) {
		if s.dedup.Seen(msg.EventID) {
			return
		}

		log := slog.With("platform", msg.Platform, "user", msg.UserID, "chat", msg.ChatID)
		trimmed := strings.TrimSpace(msg.Text)

		// Handle /cron commands
		if trimmed == "/cron" || strings.HasPrefix(trimmed, "/cron ") {
			if s.scheduler != nil {
				s.handleCronCommand(ctx, msg, trimmed, log)
			}
			return
		}

		// Handle /help command
		if trimmed == "/help" {
			if p := s.platforms[msg.Platform]; p != nil {
				help := "可用命令:\n" +
					"  /help — 显示此帮助\n" +
					"  /new [agent] — 重置会话\n" +
					"  /clear — 重置会话（同 /new）\n" +
					"  /cd <路径> — 切换工作目录\n" +
					"  /pwd — 显示当前工作目录\n" +
					"  /project [name|off|list] — 项目绑定\n" +
					"  /cron <add|list|del|pause|resume> — 定时任务"
				if len(s.agentCommands) > 0 {
					help += "\n\n可用 Agent:"
					for cmd, agentID := range s.agentCommands {
						help += "\n  /" + cmd + " → " + agentID
					}
				}
				p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: help})
			}
			return
		}

		// Handle /cd <path> to change working directory
		if strings.HasPrefix(trimmed, "/cd ") {
			// Block /cd when chat is bound to a project
			if s.projectMgr != nil {
				if proj := s.projectMgr.ProjectForChat(msg.Platform, msg.ChatType, msg.ChatID); proj != nil {
					if p := s.platforms[msg.Platform]; p != nil {
						p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: fmt.Sprintf("当前已绑定项目 %s，工作目录固定为项目路径。如需切换，请先 /project off 解绑。", proj.Name)})
					}
					return
				}
			}
			s.handleCdCommand(ctx, msg, trimmed, log)
			return
		}

		// Handle /pwd to show current working directory
		if trimmed == "/pwd" {
			chatKey := session.ChatKey(msg.Platform, msg.ChatType, msg.ChatID)
			ws := s.router.GetWorkspace(chatKey)
			if p := s.platforms[msg.Platform]; p != nil {
				p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "当前工作目录: " + ws})
			}
			return
		}

		// Handle /project [name|off] command
		if trimmed == "/project" || strings.HasPrefix(trimmed, "/project ") {
			s.handleProjectCommand(ctx, msg, trimmed, log)
			return
		}

		// Handle /new [agent] reset command
		// /clear is a Claude Code built-in that doesn't work in stream-json mode,
		// so we alias it to /new for equivalent behavior.
		if trimmed == "/new" || strings.HasPrefix(trimmed, "/new ") ||
			trimmed == "/clear" || strings.HasPrefix(trimmed, "/clear ") {
			agentToReset := ""
			if parts := strings.SplitN(trimmed, " ", 2); len(parts) > 1 {
				agentToReset = parts[1]
			}

			// In project-bound mode: /new resets planner, /new {agent} resets that agent
			if s.projectMgr != nil {
				if proj := s.projectMgr.ProjectForChat(msg.Platform, msg.ChatType, msg.ChatID); proj != nil {
					if agentToReset == "" {
						// Reset planner
						s.router.Reset(proj.PlannerSessionKey())
						if p := s.platforms[msg.Platform]; p != nil {
							p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "项目 " + proj.Name + " 的 planner 已重置。"})
						}
					} else {
						// Reset specific agent
						if id, ok := s.agentCommands[agentToReset]; ok {
							key := session.SessionKey(msg.Platform, msg.ChatType, msg.ChatID, id)
							s.router.Reset(key)
							if p := s.platforms[msg.Platform]; p != nil {
								p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "会话已重置 (" + id + ")。"})
							}
						} else if p := s.platforms[msg.Platform]; p != nil {
							p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "未知的 agent: " + agentToReset})
						}
					}
					return
				}
			}

			// Original behavior (not project-bound)
			agentID := "general"
			if agentToReset != "" {
				if id, ok := s.agentCommands[agentToReset]; ok {
					agentID = id
				} else {
					// Try reverse lookup: user may pass agent ID directly (e.g. /new code-reviewer)
					found := false
					for _, id := range s.agentCommands {
						if id == agentToReset {
							agentID = id
							found = true
							break
						}
					}
					if !found {
						if p := s.platforms[msg.Platform]; p != nil {
							errMsg := "未知的 agent: " + agentToReset
							if len(s.agentCommands) > 0 {
								var names []string
								for cmd := range s.agentCommands {
									names = append(names, cmd)
								}
								errMsg += "\n可用: " + strings.Join(names, ", ")
							}
							p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: errMsg})
						}
						return
					}
				}
			}
			key := session.SessionKey(msg.Platform, msg.ChatType, msg.ChatID, agentID)
			s.router.Reset(key)
			if p := s.platforms[msg.Platform]; p != nil {
				label := ""
				if agentID != "general" {
					label = " (" + agentID + ")"
				}
				p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "对话已重置" + label + "。"})
			}
			log.Info("session reset by user", "agent", agentID)
			return
		}

		// Resolve agent from command prefix (e.g. "/review code" -> agent=code-reviewer, text="code")
		agentID, cleanText := routing.ResolveAgent(trimmed, s.agentCommands)
		if cleanText == "" && len(msg.Images) == 0 {
			if agentID != "general" {
				if p := s.platforms[msg.Platform]; p != nil {
					p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "请在指令后输入内容。"})
				}
			}
			return
		}

		// Warn about unrecognized slash commands (likely typos)
		// Skip paths like /home/user/... (contain slash after the leading one)
		if agentID == "general" && strings.HasPrefix(cleanText, "/") {
			cmd := strings.SplitN(cleanText, " ", 2)[0]
			if !strings.Contains(cmd[1:], "/") {
				if p := s.platforms[msg.Platform]; p != nil {
					p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "未知命令: " + cmd + "\n输入 /help 查看可用命令，或直接发送消息。"})
				}
				return
			}
		}

		// Determine session key and opts: project-bound chat routes to planner
		var key string
		opts := s.agents[agentID] // zero value = use router defaults

		if s.projectMgr != nil {
			if proj := s.projectMgr.ProjectForChat(msg.Platform, msg.ChatType, msg.ChatID); proj != nil {
				if agentID == "general" {
					// Plain messages -> planner
					key = proj.PlannerSessionKey()
					opts.Exempt = true
					opts.Workspace = proj.Path
					if m := s.projectMgr.EffectivePlannerModel(proj); m != "" {
						opts.Model = m
					}
					if p := s.projectMgr.EffectivePlannerPrompt(proj); p != "" {
						// Cap-trick to isolate from shared backing array
						opts.ExtraArgs = append(opts.ExtraArgs[:len(opts.ExtraArgs):len(opts.ExtraArgs)], "--append-system-prompt", p)
					}
				} else {
					// Agent commands -> per-chat session with project workspace
					key = session.SessionKey(msg.Platform, msg.ChatType, msg.ChatID, agentID)
					opts.Workspace = proj.Path
				}
			}
		}
		if key == "" {
			key = session.SessionKey(msg.Platform, msg.ChatType, msg.ChatID, agentID)
		}

		// H1: Prevent goroutine accumulation — only one message per session at a time
		if !s.sessionGuard.TryAcquire(key) {
			if p := s.platforms[msg.Platform]; p != nil {
				if s.sessionGuard.ShouldSendWait(key) {
					p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "正在处理上一条消息，请稍候..."})
				}
			}
			return
		}
		defer s.sessionGuard.Release(key)

		// H2: Transparently adopt an external Claude session if one exists for
		// this workspace. Only fires when no managed session exists yet.
		autoResumed := s.tryAutoTakeover(ctx, session.ChatKey(msg.Platform, msg.ChatType, msg.ChatID), key, opts)

		sess, sessStatus, err := s.router.GetOrCreate(ctx, key, opts)
		if err != nil {
			log.Error("get session", "err", err)
			if p := s.platforms[msg.Platform]; p != nil {
				errStr := err.Error()
				var errMsg string
				switch {
				case strings.Contains(errStr, "max concurrent"):
					errMsg = "当前处理已满，请稍后重试。"
				case strings.Contains(errStr, "context canceled"):
					errMsg = "系统正在重启，请稍后重试。"
				default:
					errMsg = "会话创建失败，请发送 /new 重置后重试。"
				}
				p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: errMsg})
			}
			return
		}

		p := s.platforms[msg.Platform]
		if p == nil {
			log.Error("unknown platform")
			return
		}

		// Notify user: auto-resumed from external session, or fresh context lost
		if autoResumed && platform.SupportsInterimMessages(p) {
			p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "已恢复上次会话。"})
		} else if sessStatus == session.SessionNew && platform.SupportsInterimMessages(p) {
			p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "新会话已创建（之前的上下文已失效）。"})
		}

		// Status tracking: accumulate event lines and push to IM
		var (
			statusLines    []string
			thinkingMsgID  string
			lastStatusEdit time.Time
			msgIDReady     = make(chan struct{})
		)
		var thinkingSent sync.Once

		if !platform.SupportsInterimMessages(p) {
			close(msgIDReady)
		}

		onEvent := func(ev cli.Event) {
			if !platform.SupportsInterimMessages(p) {
				return
			}

			line := formatEventLine(ev)
			if line == "" {
				line = "💭 思考中..."
			}

			statusLines = appendStatusLine(statusLines, line)
			text := strings.Join(statusLines, "\n")

			// First event: send status message async
			thinkingSent.Do(func() {
				lastStatusEdit = time.Now()
				snapshot := text
				go func() {
					defer close(msgIDReady)
					id, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: snapshot})
					if err == nil {
						thinkingMsgID = id
					}
				}()
			})

			// Subsequent events: rate-limited edit
			select {
			case <-msgIDReady:
				if thinkingMsgID != "" && time.Since(lastStatusEdit) >= 1*time.Second {
					if err := p.EditMessage(ctx, thinkingMsgID, text); err == nil {
						lastStatusEdit = time.Now()
					}
				}
			default:
			}
		}

		// Convert platform images to CLI image data
		var images []cli.ImageData
		for _, img := range msg.Images {
			images = append(images, cli.ImageData{Data: img.Data, MimeType: img.MimeType})
		}

		log.Info("message received", "agent", agentID, "text_len", len(cleanText), "images", len(images))

		if s.hub != nil {
			s.hub.broadcastState(key, "running", "")
		}

		result, err := sess.Send(ctx, cleanText, images, onEvent)

		// Broadcast state to dashboard after Send completes
		if s.hub != nil {
			if rs := s.router.GetSession(key); rs != nil {
				snap := rs.Snapshot()
				s.hub.broadcastState(key, snap.State, snap.DeathReason)
			}
			s.hub.BroadcastSessionsUpdate()
		}

		if err != nil {
			log.Error("send to claude", "err", err)
			var errMsg string
			switch {
			case errors.Is(err, cli.ErrNoOutputTimeout):
				s.watchdogNoOutputKills.Add(1)
				errMsg = fmt.Sprintf("⏱️ 处理超时（%s 无输出），请简化任务后重试。", formatChineseDuration(s.noOutputTimeout))
			case errors.Is(err, cli.ErrTotalTimeout):
				s.watchdogTotalKills.Add(1)
				errMsg = fmt.Sprintf("⏱️ 处理超时（总耗时超过 %s），请拆分为更小的任务。", formatChineseDuration(s.totalTimeout))
			default:
				errMsg = "处理失败，请发送 /new 重置后重试。"
			}
			platform.ReplyWithRetry(ctx, p, platform.OutgoingMessage{ChatID: msg.ChatID, Text: errMsg}, 3)
			return
		}

		log.Info("message replied", "result_len", len(result.Text), "cost", result.CostUSD)

		// Append backend tag to reply
		replyText := result.Text + "\n\n— " + s.backendTag
		var outImages []platform.Image
		for _, path := range extractImagePaths(replyText) {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			outImages = append(outImages, platform.Image{Data: data, MimeType: mimeFromPath(path)})
			replyText = strings.ReplaceAll(replyText, path, "[图片]")
		}

		// Wait for status message to be sent before reading thinkingMsgID.
		// This must be a blocking receive to establish a happens-before
		// relationship with the goroutine that writes thinkingMsgID.
		<-msgIDReady

		// Edit status to final result, or send new message
		if replyText != "" {
			if thinkingMsgID != "" {
				if err := p.EditMessage(ctx, thinkingMsgID, replyText); err != nil {
					slog.Warn("edit message failed, sending new", "err", err)
					s.sendSplitReply(ctx, p, msg.ChatID, replyText)
				}
			} else {
				s.sendSplitReply(ctx, p, msg.ChatID, replyText)
			}
		}

		// Send extracted images
		for _, img := range outImages {
			if _, err := p.Reply(ctx, platform.OutgoingMessage{
				ChatID: msg.ChatID,
				Images: []platform.Image{img},
			}); err != nil {
				slog.Warn("send image failed", "err", err)
			}
		}
	}
}

// sendSplitReply sends a reply, splitting into multiple messages if too long.
// Each chunk is retried up to 3 times with exponential backoff.
func (s *Server) sendSplitReply(ctx context.Context, p platform.Platform, chatID, text string) {
	maxLen := p.MaxReplyLength()
	if maxLen <= 0 {
		maxLen = 4000
	}

	chunks := platform.SplitText(text, maxLen)
	total := len(chunks)
	for i, chunk := range chunks {
		if total > 1 {
			chunk += fmt.Sprintf("\n— [%d/%d]", i+1, total)
		}
		if _, err := platform.ReplyWithRetry(ctx, p, platform.OutgoingMessage{ChatID: chatID, Text: chunk}, 3); err != nil {
			slog.Error("reply chunk failed after retries", "chat", chatID, "chunk", i+1, "err", err)
		}
	}
}

// formatChineseDuration formats a duration into a short Chinese string for user messages.
// Examples: 2m → "2 分钟", 30m → "30 分钟", 4h → "4 小时", 90s → "90 秒".
func formatChineseDuration(d time.Duration) string {
	if d <= 0 {
		return "未知"
	}
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%d 小时", int(d.Hours()))
	}
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
	return fmt.Sprintf("%d 秒", int(d.Seconds()))
}

// handleCronCommand dispatches /cron subcommands (add, list, del, pause, resume).
func (s *Server) handleCronCommand(ctx context.Context, msg platform.IncomingMessage, trimmed string, log *slog.Logger) {
	p := s.platforms[msg.Platform]
	if p == nil {
		return
	}
	reply := func(text string) {
		p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: text})
	}

	// Parse subcommand: /cron <sub> [args...]
	parts := strings.SplitN(trimmed, " ", 3)
	sub := ""
	if len(parts) >= 2 {
		sub = parts[1]
	}

	switch sub {
	case "add":
		if len(parts) < 3 {
			reply("用法: /cron add \"<schedule>\" <prompt>\n例: /cron add \"@every 30m\" 检查服务状态")
			return
		}
		schedule, prompt, err := parseCronAdd(parts[2])
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
		if err := s.scheduler.AddJob(job); err != nil {
			reply("创建失败: " + err.Error())
			return
		}
		next := s.scheduler.NextRun(job)
		reply(fmt.Sprintf("Job %s 已创建。Schedule: %s, Next: %s", job.ID, job.Schedule, next.Format("01/02 15:04")))
		log.Info("cron job created", "id", job.ID, "schedule", job.Schedule)

	case "list":
		jobs := s.scheduler.ListJobs(msg.Platform, msg.ChatID)
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
			sb.WriteString(fmt.Sprintf("  %s  %-20s %s%s\n", j.ID, j.Schedule, j.Prompt, status))
		}
		reply(sb.String())

	case "del":
		if len(parts) < 3 {
			reply("用法: /cron del <id>")
			return
		}
		j, err := s.scheduler.DeleteJob(parts[2], msg.Platform, msg.ChatID)
		if err != nil {
			reply("删除失败: " + err.Error())
			return
		}
		reply(fmt.Sprintf("Job %s 已删除。", j.ID))
		log.Info("cron job deleted", "id", j.ID)

	case "pause":
		if len(parts) < 3 {
			reply("用法: /cron pause <id>")
			return
		}
		j, err := s.scheduler.PauseJob(parts[2], msg.Platform, msg.ChatID)
		if err != nil {
			reply("暂停失败: " + err.Error())
			return
		}
		reply(fmt.Sprintf("Job %s 已暂停。", j.ID))
		log.Info("cron job paused", "id", j.ID)

	case "resume":
		if len(parts) < 3 {
			reply("用法: /cron resume <id>")
			return
		}
		j, err := s.scheduler.ResumeJob(parts[2], msg.Platform, msg.ChatID)
		if err != nil {
			reply("恢复失败: " + err.Error())
			return
		}
		next := s.scheduler.NextRun(j)
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

// handleProjectCommand handles /project [name|off] commands.
func (s *Server) handleProjectCommand(ctx context.Context, msg platform.IncomingMessage, trimmed string, log *slog.Logger) {
	p := s.platforms[msg.Platform]
	if p == nil {
		return
	}

	if s.projectMgr == nil {
		p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "项目功能未启用（未配置 projects.root）。"})
		return
	}

	arg := strings.TrimSpace(strings.TrimPrefix(trimmed, "/project"))

	// /project — show current binding
	if arg == "" {
		proj := s.projectMgr.ProjectForChat(msg.Platform, msg.ChatType, msg.ChatID)
		if proj == nil {
			p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "当前未绑定项目。\n用法: /project <项目名> 绑定"})
		} else {
			p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: fmt.Sprintf("当前绑定: %s (%s)", proj.Name, proj.Path)})
		}
		return
	}

	// /project off — unbind
	if arg == "off" {
		if err := s.projectMgr.UnbindAllChat(msg.Platform, msg.ChatType, msg.ChatID); err != nil {
			p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "解绑失败: " + err.Error()})
			return
		}
		p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "已解绑项目，恢复默认路由。"})
		log.Info("project unbound", "chat", msg.ChatID)
		return
	}

	// /project list — list all projects
	if arg == "list" {
		projects := s.projectMgr.All()
		if len(projects) == 0 {
			p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "无可用项目。"})
			return
		}
		var lines []string
		for _, proj := range projects {
			lines = append(lines, fmt.Sprintf("  %s — %s", proj.Name, proj.Path))
		}
		p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "可用项目:\n" + strings.Join(lines, "\n")})
		return
	}

	// /project <name> — bind to project
	proj := s.projectMgr.Get(arg)
	if proj == nil {
		p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "项目不存在: " + arg + "\n使用 /project list 查看可用项目。"})
		return
	}

	if err := s.projectMgr.BindChat(proj.Name, msg.Platform, msg.ChatType, msg.ChatID); err != nil {
		p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "绑定失败: " + err.Error()})
		return
	}

	p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: fmt.Sprintf("已绑定项目: %s\n后续消息将路由到该项目的 planner。", proj.Name)})
	log.Info("project bound", "project", proj.Name, "chat", msg.ChatID)
}

// handleCdCommand changes the working directory for all sessions in a chat.
func (s *Server) handleCdCommand(ctx context.Context, msg platform.IncomingMessage, trimmed string, log *slog.Logger) {
	p := s.platforms[msg.Platform]
	if p == nil {
		return
	}

	path := strings.TrimSpace(strings.TrimPrefix(trimmed, "/cd"))
	if path == "" {
		p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "用法: /cd <目录路径>\n例: /cd /home/ubuntu/my-project"})
		return
	}

	// Expand ~ to home directory
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, path[1:])
		}
	}

	// Resolve relative paths against the chat's current workspace, not naozhi's cwd
	var absPath string
	if filepath.IsAbs(path) {
		absPath = filepath.Clean(path)
	} else {
		chatKey := session.ChatKey(msg.Platform, msg.ChatType, msg.ChatID)
		currentWS := s.router.GetWorkspace(chatKey)
		absPath = filepath.Join(currentWS, path)
	}

	// Verify directory exists
	info, err := os.Stat(absPath)
	if err != nil || !info.IsDir() {
		p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "目录不存在: " + absPath})
		return
	}

	// Resolve symlinks before allowedRoot check to prevent symlink bypass
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		absPath = resolved
	}

	// Enforce path whitelist
	if s.allowedRoot != "" && absPath != s.allowedRoot && !strings.HasPrefix(absPath, s.allowedRoot+string(filepath.Separator)) {
		p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "不允许访问该路径，只能在 " + s.allowedRoot + " 下操作"})
		return
	}

	chatKey := session.ChatKey(msg.Platform, msg.ChatType, msg.ChatID)
	s.router.SetWorkspace(chatKey, absPath)
	s.router.ResetChat(chatKey)

	p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "工作目录已切换到: " + absPath + "\n所有会话已重置，新消息将在此目录下执行。"})
	log.Info("workspace changed", "chat_key", chatKey, "path", absPath)
}

// parseCronAdd parses the args of /cron add: "schedule" prompt
func parseCronAdd(args string) (schedule, prompt string, err error) {
	// Expect: "schedule" prompt
	if !strings.HasPrefix(args, "\"") {
		return "", "", fmt.Errorf("schedule must be quoted, e.g. \"@every 30m\"")
	}
	end := strings.Index(args[1:], "\"")
	if end < 0 {
		return "", "", fmt.Errorf("missing closing quote for schedule")
	}
	schedule = args[1 : end+1]
	prompt = strings.TrimSpace(args[end+2:])
	if prompt == "" {
		return "", "", fmt.Errorf("prompt cannot be empty")
	}
	return schedule, prompt, nil
}
