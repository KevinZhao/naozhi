package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// dispatchCommand handles slash commands (/help, /new, /clear, /cron, /cd, /pwd, /project).
// Returns true if the message was a command and was handled.
func (d *Dispatcher) dispatchCommand(ctx context.Context, msg platform.IncomingMessage, trimmed string, log *slog.Logger) bool {
	switch {
	case trimmed == "/cron" || strings.HasPrefix(trimmed, "/cron "):
		if d.Scheduler != nil {
			d.handleCronCommand(ctx, msg, trimmed, log)
		}
		return true

	case trimmed == "/help":
		d.handleHelpCommand(ctx, msg)
		return true

	case strings.HasPrefix(trimmed, "/cd "):
		if d.ProjectMgr != nil {
			if proj := d.ProjectMgr.ProjectForChat(msg.Platform, msg.ChatType, msg.ChatID); proj != nil {
				if p := d.Platforms[msg.Platform]; p != nil {
					if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: fmt.Sprintf("当前已绑定项目 %s，工作目录固定为项目路径。如需切换，请先 /project off 解绑。", proj.Name)}); err != nil {
						slog.Warn("reply failed", "platform", msg.Platform, "chat", msg.ChatID, "err", err)
					}
				}
				return true
			}
		}
		d.handleCdCommand(ctx, msg, trimmed, log)
		return true

	case trimmed == "/pwd":
		chatKey := session.ChatKey(msg.Platform, msg.ChatType, msg.ChatID)
		ws := d.Router.GetWorkspace(chatKey)
		if p := d.Platforms[msg.Platform]; p != nil {
			if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "当前工作目录: " + ws}); err != nil {
				slog.Warn("reply failed", "platform", msg.Platform, "chat", msg.ChatID, "err", err)
			}
		}
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
	p := d.Platforms[msg.Platform]
	if p == nil {
		return
	}
	help := "可用命令:\n" +
		"  /help — 显示此帮助\n" +
		"  /new [agent] — 重置会话\n" +
		"  /clear — 重置会话（同 /new）\n" +
		"  /cd <路径> — 切换工作目录\n" +
		"  /pwd — 显示当前工作目录\n" +
		"  /project [name|off|list] — 项目绑定\n" +
		"  /cron <add|list|del|pause|resume> — 定时任务"
	if len(d.AgentCommands) > 0 {
		help += "\n\n可用 Agent:"
		for cmd, agentID := range d.AgentCommands {
			help += "\n  /" + cmd + " → " + agentID
		}
	}
	if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: help}); err != nil {
		slog.Warn("reply failed", "platform", msg.Platform, "chat", msg.ChatID, "err", err)
	}
}

func (d *Dispatcher) handleNewCommand(ctx context.Context, msg platform.IncomingMessage, trimmed string, log *slog.Logger) {
	agentToReset := ""
	if parts := strings.SplitN(trimmed, " ", 2); len(parts) > 1 {
		agentToReset = parts[1]
	}

	// In project-bound mode: /new resets planner, /new {agent} resets that agent
	if d.ProjectMgr != nil {
		if proj := d.ProjectMgr.ProjectForChat(msg.Platform, msg.ChatType, msg.ChatID); proj != nil {
			if agentToReset == "" {
				d.Router.Reset(proj.PlannerSessionKey())
				if p := d.Platforms[msg.Platform]; p != nil {
					if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "项目 " + proj.Name + " 的 planner 已重置。"}); err != nil {
						log.Warn("reply failed", "err", err)
					}
				}
			} else {
				if id, ok := d.AgentCommands[agentToReset]; ok {
					key := session.SessionKey(msg.Platform, msg.ChatType, msg.ChatID, id)
					d.Router.Reset(key)
					if p := d.Platforms[msg.Platform]; p != nil {
						if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "会话已重置 (" + id + ")。"}); err != nil {
							log.Warn("reply failed", "err", err)
						}
					}
				} else if p := d.Platforms[msg.Platform]; p != nil {
					if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "未知的 agent: " + agentToReset}); err != nil {
						log.Warn("reply failed", "err", err)
					}
				}
			}
			return
		}
	}

	agentID := "general"
	if agentToReset != "" {
		if id, ok := d.AgentCommands[agentToReset]; ok {
			agentID = id
		} else {
			found := false
			for _, id := range d.AgentCommands {
				if id == agentToReset {
					agentID = id
					found = true
					break
				}
			}
			if !found {
				if p := d.Platforms[msg.Platform]; p != nil {
					errMsg := "未知的 agent: " + agentToReset
					if len(d.AgentCommands) > 0 {
						var names []string
						for cmd := range d.AgentCommands {
							names = append(names, cmd)
						}
						errMsg += "\n可用: " + strings.Join(names, ", ")
					}
					if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: errMsg}); err != nil {
						log.Warn("reply failed", "err", err)
					}
				}
				return
			}
		}
	}
	key := session.SessionKey(msg.Platform, msg.ChatType, msg.ChatID, agentID)
	d.Router.Reset(key)
	if p := d.Platforms[msg.Platform]; p != nil {
		label := ""
		if agentID != "general" {
			label = " (" + agentID + ")"
		}
		if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "对话已重置" + label + "。"}); err != nil {
			log.Warn("reply failed", "err", err)
		}
	}
	log.Info("session reset by user", "agent", agentID)
}

// handleCronCommand dispatches /cron subcommands (add, list, del, pause, resume).
func (d *Dispatcher) handleCronCommand(ctx context.Context, msg platform.IncomingMessage, trimmed string, log *slog.Logger) {
	p := d.Platforms[msg.Platform]
	if p == nil {
		return
	}
	reply := func(text string) {
		if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: text}); err != nil {
			log.Warn("reply failed", "err", err)
		}
	}

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
		if err := d.Scheduler.AddJob(job); err != nil {
			reply("创建失败: " + err.Error())
			return
		}
		next := d.Scheduler.NextRun(job)
		reply(fmt.Sprintf("Job %s 已创建。Schedule: %s, Next: %s", job.ID, job.Schedule, next.Format("01/02 15:04")))
		log.Info("cron job created", "id", job.ID, "schedule", job.Schedule)

	case "list":
		jobs := d.Scheduler.ListJobs(msg.Platform, msg.ChatID)
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
		j, err := d.Scheduler.DeleteJob(parts[2], msg.Platform, msg.ChatID)
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
		j, err := d.Scheduler.PauseJob(parts[2], msg.Platform, msg.ChatID)
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
		j, err := d.Scheduler.ResumeJob(parts[2], msg.Platform, msg.ChatID)
		if err != nil {
			reply("恢复失败: " + err.Error())
			return
		}
		next := d.Scheduler.NextRun(j)
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
	p := d.Platforms[msg.Platform]
	if p == nil {
		return
	}

	if d.ProjectMgr == nil {
		if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "项目功能未启用（未配置 projects.root）。"}); err != nil {
			log.Warn("reply failed", "err", err)
		}
		return
	}

	arg := strings.TrimSpace(strings.TrimPrefix(trimmed, "/project"))

	switch arg {
	case "":
		proj := d.ProjectMgr.ProjectForChat(msg.Platform, msg.ChatType, msg.ChatID)
		if proj == nil {
			if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "当前未绑定项目。\n用法: /project <项目名> 绑定"}); err != nil {
				log.Warn("reply failed", "err", err)
			}
		} else {
			if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: fmt.Sprintf("当前绑定: %s (%s)", proj.Name, proj.Path)}); err != nil {
				log.Warn("reply failed", "err", err)
			}
		}

	case "off":
		if err := d.ProjectMgr.UnbindAllChat(msg.Platform, msg.ChatType, msg.ChatID); err != nil {
			if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "解绑失败: " + err.Error()}); err != nil {
				log.Warn("reply failed", "err", err)
			}
			return
		}
		if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "已解绑项目，恢复默认路由。"}); err != nil {
			log.Warn("reply failed", "err", err)
		}
		log.Info("project unbound", "chat", msg.ChatID)

	case "list":
		projects := d.ProjectMgr.All()
		if len(projects) == 0 {
			if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "无可用项目。"}); err != nil {
				log.Warn("reply failed", "err", err)
			}
			return
		}
		var lines []string
		for _, proj := range projects {
			lines = append(lines, fmt.Sprintf("  %s — %s", proj.Name, proj.Path))
		}
		if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "可用项目:\n" + strings.Join(lines, "\n")}); err != nil {
			log.Warn("reply failed", "err", err)
		}

	default:
		proj := d.ProjectMgr.Get(arg)
		if proj == nil {
			if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "项目不存在: " + arg + "\n使用 /project list 查看可用项目。"}); err != nil {
				log.Warn("reply failed", "err", err)
			}
			return
		}
		if err := d.ProjectMgr.BindChat(proj.Name, msg.Platform, msg.ChatType, msg.ChatID); err != nil {
			if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "绑定失败: " + err.Error()}); err != nil {
				log.Warn("reply failed", "err", err)
			}
			return
		}
		if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: fmt.Sprintf("已绑定项目: %s\n后续消息将路由到该项目的 planner。", proj.Name)}); err != nil {
			log.Warn("reply failed", "err", err)
		}
		log.Info("project bound", "project", proj.Name, "chat", msg.ChatID)
	}
}

// handleCdCommand changes the working directory for all sessions in a chat.
func (d *Dispatcher) handleCdCommand(ctx context.Context, msg platform.IncomingMessage, trimmed string, log *slog.Logger) {
	p := d.Platforms[msg.Platform]
	if p == nil {
		return
	}

	path := strings.TrimSpace(strings.TrimPrefix(trimmed, "/cd"))
	if path == "" {
		if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "用法: /cd <目录路径>\n例: /cd /home/ubuntu/my-project"}); err != nil {
			log.Warn("reply failed", "err", err)
		}
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
		currentWS := d.Router.GetWorkspace(chatKey)
		absPath = filepath.Join(currentWS, path)
	}

	info, err := os.Stat(absPath)
	if err != nil || !info.IsDir() {
		if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "目录不存在或无权限"}); err != nil {
			log.Warn("reply failed", "err", err)
		}
		return
	}

	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "无法解析路径"}); err != nil {
			log.Warn("reply failed", "err", err)
		}
		return
	}
	absPath = resolved

	if d.AllowedRoot != "" && absPath != d.AllowedRoot && !strings.HasPrefix(absPath, d.AllowedRoot+string(filepath.Separator)) {
		if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "不允许访问该路径"}); err != nil {
			log.Warn("reply failed", "err", err)
		}
		return
	}

	chatKey := session.ChatKey(msg.Platform, msg.ChatType, msg.ChatID)
	d.Router.SetWorkspace(chatKey, absPath)
	d.Router.ResetChat(chatKey)

	if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "工作目录已切换到: " + absPath + "\n所有会话已重置，新消息将在此目录下执行。"}); err != nil {
		log.Warn("reply failed", "err", err)
	}
	log.Info("workspace changed", "chat_key", chatKey, "path", absPath)
}

// ParseCronAdd parses the args of /cron add: "schedule" prompt
func ParseCronAdd(args string) (schedule, prompt string, err error) {
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
