package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	addr           string
	mux            *http.ServeMux
	platforms      map[string]platform.Platform
	router         *session.Router
	dedup          *platform.Dedup
	sessionGuard   *sessionGuard
	startedAt      time.Time
	agents         map[string]session.AgentOpts
	agentCommands  map[string]string
	scheduler      *cron.Scheduler
	backendTag     string // e.g., "cc" or "kiro", appended to replies
	dashboardToken string // optional bearer token for dashboard API
	hub            *Hub   // WebSocket hub
	nodes          map[string]*NodeClient
	claudeDir      string // path to ~/.claude for session discovery
}

// sessionGuard prevents multiple concurrent messages to the same session.
type sessionGuard struct {
	mu     sync.Mutex
	active map[string]struct{}
}

func newSessionGuard() *sessionGuard {
	return &sessionGuard{active: make(map[string]struct{})}
}

func (g *sessionGuard) TryAcquire(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.active[key]; ok {
		return false
	}
	g.active[key] = struct{}{}
	return true
}

func (g *sessionGuard) Release(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.active, key)
}

// New creates a new Server.
func New(addr string, router *session.Router, platforms map[string]platform.Platform, agents map[string]session.AgentOpts, agentCommands map[string]string, scheduler *cron.Scheduler, backend string) *Server {
	tag := "cc"
	if backend == "kiro" {
		tag = "kiro"
	}
	claudeDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		claudeDir = filepath.Join(home, ".claude")
	}
	return &Server{
		addr:          addr,
		mux:           http.NewServeMux(),
		platforms:     platforms,
		router:        router,
		dedup:         platform.NewDedup(defaultDedupCapacity),
		sessionGuard:  newSessionGuard(),
		startedAt:     time.Now(),
		agents:        agents,
		agentCommands: agentCommands,
		scheduler:     scheduler,
		backendTag:    tag,
		claudeDir:     claudeDir,
	}
}

// SetDashboardToken sets the optional bearer token required for dashboard send API.
func (s *Server) SetDashboardToken(token string) {
	s.dashboardToken = token
}

// SetNodes configures remote node clients for multi-node aggregation.
func (s *Server) SetNodes(nodes map[string]*NodeClient) {
	s.nodes = nodes
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
	s.registerDashboard()
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

		// Shutdown WebSocket hub
		if s.hub != nil {
			s.hub.Shutdown()
		}

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

		// Handle /cd <path> to change working directory
		if strings.HasPrefix(trimmed, "/cd ") {
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

		// Handle /new [agent] reset command
		// /clear is a Claude Code built-in that doesn't work in stream-json mode,
		// so we alias it to /new for equivalent behavior.
		if trimmed == "/new" || strings.HasPrefix(trimmed, "/new ") ||
			trimmed == "/clear" {
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

		// H1: Prevent goroutine accumulation — only one message per session at a time
		if !s.sessionGuard.TryAcquire(key) {
			if p := s.platforms[msg.Platform]; p != nil {
				p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "正在处理上一条消息，请稍候..."})
			}
			return
		}
		defer s.sessionGuard.Release(key)

		sess, sessStatus, err := s.router.GetOrCreate(ctx, key, opts)
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

		// Notify user when previous session context was lost
		if sessStatus == session.SessionNew && platform.SupportsInterimMessages(p) {
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
				if thinkingMsgID != "" && time.Since(lastStatusEdit) >= 2*time.Second {
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

func splitText(text string, maxRunes int) []string {
	if utf8.RuneCountInString(text) <= maxRunes {
		return []string{text}
	}
	var chunks []string
	for text != "" {
		if utf8.RuneCountInString(text) <= maxRunes {
			chunks = append(chunks, text)
			break
		}
		// Advance maxRunes runes to find byte offset
		end := 0
		for i := 0; i < maxRunes && end < len(text); i++ {
			_, size := utf8.DecodeRuneInString(text[end:])
			end += size
		}
		// Prefer splitting at a newline in the second half
		if idx := strings.LastIndex(text[:end], "\n"); idx > end/2 {
			end = idx + 1
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
