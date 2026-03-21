package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/routing"
	"github.com/naozhi/naozhi/internal/session"
)

const (
	defaultDedupCapacity = 10000
	shutdownTimeout      = 30 * time.Second
)

// Server is the HTTP entry point for Naozhi.
type Server struct {
	addr          string
	mux           *http.ServeMux
	platforms     map[string]platform.Platform
	router        *session.Router
	dedup         *platform.Dedup
	startedAt     time.Time
	agents        map[string]session.AgentOpts
	agentCommands map[string]string
	scheduler     *cron.Scheduler
	backendTag    string // e.g., "cc" or "kiro", appended to replies
}

// New creates a new Server.
func New(addr string, router *session.Router, platforms map[string]platform.Platform, agents map[string]session.AgentOpts, agentCommands map[string]string, scheduler *cron.Scheduler, backend string) *Server {
	tag := "cc"
	if backend == "kiro" {
		tag = "kiro"
	}
	return &Server{
		addr:          addr,
		mux:           http.NewServeMux(),
		platforms:     platforms,
		router:        router,
		dedup:         platform.NewDedup(defaultDedupCapacity),
		startedAt:     time.Now(),
		agents:        agents,
		agentCommands: agentCommands,
		scheduler:     scheduler,
		backendTag:    tag,
	}
}

// Start registers routes and begins serving.
func (s *Server) Start(ctx context.Context) error {
	handler := s.buildMessageHandler()

	for _, p := range s.platforms {
		p.RegisterRoutes(s.mux, handler)
		slog.Info("platform registered", "name", p.Name())

		if rp, ok := p.(platform.RunnablePlatform); ok {
			if err := rp.Start(handler); err != nil {
				return fmt.Errorf("start platform %s: %w", p.Name(), err)
			}
		}
	}

	s.mux.HandleFunc("GET /health", s.handleHealth)
	slog.Info("server starting", "addr", s.addr)

	srv := &http.Server{
		Addr:         s.addr,
		Handler:      s.mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		slog.Info("shutting down server")

		// Stop RunnablePlatforms (e.g. WebSocket connections)
		for _, p := range s.platforms {
			if rp, ok := p.(platform.RunnablePlatform); ok {
				if err := rp.Stop(); err != nil {
					slog.Error("stop platform", "name", p.Name(), "err", err)
				}
			}
		}

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("server shutdown error", "err", err)
		}
	}()

	return srv.ListenAndServe()
}

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

		// Handle /new [agent] reset command
		if trimmed == "/new" || strings.HasPrefix(trimmed, "/new ") {
			agentToReset := "general"
			if parts := strings.SplitN(trimmed, " ", 2); len(parts) > 1 {
				if id, ok := s.agentCommands[parts[1]]; ok {
					agentToReset = id
				} else {
					if p := s.platforms[msg.Platform]; p != nil {
						p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "未知的 agent: " + parts[1]})
					}
					return
				}
			}
			key := session.SessionKey(msg.Platform, msg.ChatType, msg.ChatID, agentToReset)
			s.router.Reset(key)
			if p := s.platforms[msg.Platform]; p != nil {
				label := ""
				if agentToReset != "general" {
					label = " (" + agentToReset + ")"
				}
				p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "对话已重置" + label + "。"})
			}
			log.Info("session reset by user", "agent", agentToReset)
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

		opts := s.agents[agentID] // zero value = use router defaults
		key := session.SessionKey(msg.Platform, msg.ChatType, msg.ChatID, agentID)

		sess, err := s.router.GetOrCreate(ctx, key, opts)
		if err != nil {
			log.Error("get session", "err", err)
			if p := s.platforms[msg.Platform]; p != nil {
				p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "会话异常，请发送 /new 重置后重试。"})
			}
			return
		}

		p := s.platforms[msg.Platform]
		if p == nil {
			log.Error("unknown platform")
			return
		}

		// Send "thinking..." indicator
		var thinkingMsgID string
		var thinkingSent sync.Once

		onEvent := func(ev cli.Event) {
			thinkingSent.Do(func() {
				id, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "思考中..."})
				if err == nil {
					thinkingMsgID = id
				}
			})
		}

		// Convert platform images to CLI image data
		var images []cli.ImageData
		for _, img := range msg.Images {
			images = append(images, cli.ImageData{Data: img.Data, MimeType: img.MimeType})
		}

		log.Info("message received", "agent", agentID, "text_len", len(cleanText), "images", len(images))

		result, err := sess.Send(ctx, cleanText, images, onEvent)
		if err != nil {
			log.Error("send to claude", "err", err)
			p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "处理失败，请发送 /new 重置后重试。"})
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

		// Edit "thinking..." to final result, or send new message
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
func (s *Server) sendSplitReply(ctx context.Context, p platform.Platform, chatID, text string) {
	maxLen := p.MaxReplyLength()
	if maxLen <= 0 {
		maxLen = 4000
	}

	chunks := splitText(text, maxLen)
	for _, chunk := range chunks {
		if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: chatID, Text: chunk}); err != nil {
			slog.Error("reply failed", "err", err)
		}
	}
}

func splitText(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		end := maxLen
		if end > len(text) {
			end = len(text)
		}
		// Try to split at newline
		if end < len(text) {
			if idx := strings.LastIndex(text[:end], "\n"); idx > maxLen/2 {
				end = idx + 1
			}
		}
		chunks = append(chunks, text[:end])
		text = text[end:]
	}
	return chunks
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	active, total := s.router.Stats()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "ok",
		"uptime":   time.Since(s.startedAt).Round(time.Second).String(),
		"sessions": map[string]int{"active": active, "total": total},
	}); err != nil {
		slog.Error("encode health response", "err", err)
	}
}

// handleCronCommand dispatches /cron subcommands.
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
