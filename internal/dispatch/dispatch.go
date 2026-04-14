package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/routing"
	"github.com/naozhi/naozhi/internal/session"
)

// SessionGuard prevents multiple concurrent messages to the same session.
type SessionGuard interface {
	TryAcquire(key string) bool
	ShouldSendWait(key string) bool
	Release(key string)
}

// Dispatcher holds the dependencies needed to dispatch incoming IM messages
// to the session router, handle slash commands, and stream results back.
type Dispatcher struct {
	Router        *session.Router
	Platforms     map[string]platform.Platform
	Agents        map[string]session.AgentOpts
	AgentCommands map[string]string
	Scheduler     *cron.Scheduler
	ProjectMgr    *project.Manager
	Guard         SessionGuard
	Dedup         *platform.Dedup
	AllowedRoot   string
	ClaudeDir     string
	BackendTag    string

	NoOutputTimeout       time.Duration
	TotalTimeout          time.Duration
	WatchdogNoOutputKills *atomic.Int64
	WatchdogTotalKills    *atomic.Int64

	SendFn     func(ctx context.Context, key string, sess *session.ManagedSession, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error)
	TakeoverFn func(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool
}

// BuildHandler returns a platform.MessageHandler wired to this Dispatcher.
func (d *Dispatcher) BuildHandler() platform.MessageHandler {
	return func(ctx context.Context, msg platform.IncomingMessage) {
		// Dedup check at the top prevents duplicate processing from platform
		// retries (e.g., Feishu webhook timeout → re-delivery with same event_id).
		// Note: if guard fails below, the eventID is still consumed. This means
		// a platform retry during guard contention won't be re-processed. In
		// practice this is benign — the handler responds fast enough that
		// platforms don't retry, and the user is told to resend.
		if d.Dedup.Seen(msg.EventID) {
			return
		}

		log := slog.With("platform", msg.Platform, "user", msg.UserID, "chat", msg.ChatID)
		trimmed := strings.TrimSpace(msg.Text)

		// Dispatch slash commands (/help, /new, /cron, /cd, /pwd, /project)
		if d.dispatchCommand(ctx, msg, trimmed, log) {
			return
		}

		// Resolve agent from command prefix (e.g. "/review code" -> agent=code-reviewer, text="code")
		agentID, cleanText := routing.ResolveAgent(trimmed, d.AgentCommands)
		if cleanText == "" && len(msg.Images) == 0 {
			if agentID != "general" {
				if p := d.Platforms[msg.Platform]; p != nil {
					if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "请在指令后输入内容。"}); err != nil {
						log.Warn("reply failed", "err", err)
					}
				}
			}
			return
		}

		// Warn about unrecognized slash commands (likely typos)
		// Skip paths like /home/user/... (contain slash after the leading one)
		if agentID == "general" && strings.HasPrefix(cleanText, "/") {
			cmd := strings.SplitN(cleanText, " ", 2)[0]
			if !strings.Contains(cmd[1:], "/") {
				if p := d.Platforms[msg.Platform]; p != nil {
					if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "未知命令: " + cmd + "\n输入 /help 查看可用命令，或直接发送消息。"}); err != nil {
						log.Warn("reply failed", "err", err)
					}
				}
				return
			}
		}

		// Determine session key and opts: project-bound chat routes to planner
		var key string
		opts := d.Agents[agentID] // zero value = use router defaults

		if d.ProjectMgr != nil {
			if proj := d.ProjectMgr.ProjectForChat(msg.Platform, msg.ChatType, msg.ChatID); proj != nil {
				if agentID == "general" {
					key = proj.PlannerSessionKey()
					opts.Exempt = true
					opts.Workspace = proj.Path
					if m := d.ProjectMgr.EffectivePlannerModel(proj); m != "" {
						opts.Model = m
					}
					if p := d.ProjectMgr.EffectivePlannerPrompt(proj); p != "" {
						opts.ExtraArgs = append(opts.ExtraArgs[:len(opts.ExtraArgs):len(opts.ExtraArgs)], "--append-system-prompt", p)
					}
				} else {
					key = session.SessionKey(msg.Platform, msg.ChatType, msg.ChatID, agentID)
					opts.Workspace = proj.Path
				}
			}
		}
		if key == "" {
			key = session.SessionKey(msg.Platform, msg.ChatType, msg.ChatID, agentID)
		}

		// H1: Prevent goroutine accumulation — only one message per session at a time
		if !d.Guard.TryAcquire(key) {
			if p := d.Platforms[msg.Platform]; p != nil {
				if d.Guard.ShouldSendWait(key) {
					if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "正在处理上一条消息，请稍候..."}); err != nil {
						log.Warn("reply failed", "err", err)
					}
				}
			}
			return
		}
		defer d.Guard.Release(key)
		defer d.Router.NotifyIdle()

		// H2: Transparently adopt an external Claude session if one exists for
		// this workspace. Only fires when no managed session exists yet.
		autoResumed := d.TakeoverFn(ctx, session.ChatKey(msg.Platform, msg.ChatType, msg.ChatID), key, opts)

		sess, sessStatus, err := d.Router.GetOrCreate(ctx, key, opts)
		if err != nil {
			log.Error("get session", "err", err)
			if p := d.Platforms[msg.Platform]; p != nil {
				var errMsg string
				switch {
				case errors.Is(err, session.ErrMaxProcs):
					errMsg = "当前处理已满，请稍后重试。"
				case errors.Is(err, context.Canceled):
					errMsg = "系统正在重启，请稍后重试。"
				default:
					errMsg = "会话创建失败，请发送 /new 重置后重试。"
				}
				if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: errMsg}); err != nil {
					log.Warn("reply failed", "err", err)
				}
			}
			return
		}

		p := d.Platforms[msg.Platform]
		if p == nil {
			log.Error("unknown platform")
			return
		}

		// Notify user: auto-resumed from external session, or fresh context lost
		if autoResumed && platform.SupportsInterimMessages(p) {
			if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "已恢复上次会话。"}); err != nil {
				log.Warn("reply failed", "err", err)
			}
		} else if sessStatus == session.SessionNew && platform.SupportsInterimMessages(p) {
			if _, err := p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "新会话已创建（之前的上下文已失效）。"}); err != nil {
				log.Warn("reply failed", "err", err)
			}
		}

		// Build IM event tracker for streaming status updates
		tracker := newIMEventTracker(ctx, p, msg.ChatID)

		// Convert platform images to CLI image data
		var images []cli.ImageData
		for _, img := range msg.Images {
			images = append(images, cli.ImageData{Data: img.Data, MimeType: img.MimeType})
		}

		log.Info("message received", "agent", agentID, "text_len", len(cleanText), "images", len(images))

		result, err := d.SendFn(ctx, key, sess, cleanText, images, tracker.onEvent)

		if err != nil {
			log.Error("send to claude", "err", err)
			var errMsg string
			switch {
			case errors.Is(err, cli.ErrNoOutputTimeout):
				d.WatchdogNoOutputKills.Add(1)
				errMsg = fmt.Sprintf("⏱️ 处理超时（%s 无输出），请简化任务后重试。", formatChineseDuration(d.NoOutputTimeout))
			case errors.Is(err, cli.ErrTotalTimeout):
				d.WatchdogTotalKills.Add(1)
				errMsg = fmt.Sprintf("⏱️ 处理超时（总耗时超过 %s），请拆分为更小的任务。", formatChineseDuration(d.TotalTimeout))
			default:
				errMsg = "处理失败，请发送 /new 重置后重试。"
			}
			if _, err := platform.ReplyWithRetry(ctx, p, platform.OutgoingMessage{ChatID: msg.ChatID, Text: errMsg}, 3); err != nil {
				log.Warn("error reply also failed", "chat", msg.ChatID, "err", err)
			}
			return
		}

		log.Info("message replied", "result_len", len(result.Text), "cost", result.CostUSD)

		// Append backend tag to reply
		replyText := result.Text
		if d.BackendTag != "" {
			replyText += "\n\n— " + d.BackendTag
		}
		var outImages []platform.Image
		for _, path := range cli.ExtractImagePaths(replyText) {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			outImages = append(outImages, platform.Image{Data: data, MimeType: cli.MimeFromPath(path)})
			replyText = strings.ReplaceAll(replyText, path, "[图片]")
		}

		// Wait for status message to be sent before reading thinkingMsgID.
		tracker.waitReady()

		// Edit status to final result, or send new message
		if replyText != "" {
			if tracker.thinkingMsgID != "" {
				if err := p.EditMessage(ctx, tracker.thinkingMsgID, replyText); err != nil {
					slog.Warn("edit message failed, sending new", "err", err)
					d.SendSplitReply(ctx, p, msg.ChatID, replyText)
				}
			} else {
				d.SendSplitReply(ctx, p, msg.ChatID, replyText)
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

// SendSplitReply sends a reply, splitting into multiple messages if too long.
func (d *Dispatcher) SendSplitReply(ctx context.Context, p platform.Platform, chatID, text string) {
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

// formatChineseDuration formats a duration into a short Chinese string.
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

// imEventTracker manages IM status message streaming (thinking -> tool_use -> result).
type imEventTracker struct {
	ctx           context.Context
	p             platform.Platform
	chatID        string
	statusLines   []string
	thinkingMsgID string
	msgIDReady    chan struct{}
	sent          sync.Once
	editCh        chan string // buffered(1), coalescing; editLoop drains and rate-limits
}

func newIMEventTracker(ctx context.Context, p platform.Platform, chatID string) *imEventTracker {
	t := &imEventTracker{
		ctx:        ctx,
		p:          p,
		chatID:     chatID,
		msgIDReady: make(chan struct{}),
		editCh:     make(chan string, 1),
	}
	if !platform.SupportsInterimMessages(p) {
		t.sent.Do(func() {
			close(t.msgIDReady)
		})
	} else {
		go t.editLoop()
	}
	return t
}

func (t *imEventTracker) onEvent(ev cli.Event) {
	if !platform.SupportsInterimMessages(t.p) {
		return
	}

	line := formatEventLine(ev)
	if line == "" {
		line = "💭 思考中..."
	}

	t.statusLines = appendStatusLine(t.statusLines, line)
	text := strings.Join(t.statusLines, "\n")

	t.sent.Do(func() {
		snapshot := text
		go func() {
			defer close(t.msgIDReady)
			id, err := t.p.Reply(t.ctx, platform.OutgoingMessage{ChatID: t.chatID, Text: snapshot})
			if err == nil {
				t.thinkingMsgID = id
			}
		}()
	})

	// Non-blocking: queue the latest text for async editing.
	// The channel holds at most 1 pending edit; new text replaces stale.
	select {
	case t.editCh <- text:
	default:
		select {
		case <-t.editCh:
		default:
		}
		select {
		case t.editCh <- text:
		default:
		}
	}
}

// editLoop runs in a goroutine and rate-limits EditMessage calls to 1/s.
// This keeps onEvent non-blocking so Process.Send can drain eventCh at full speed.
func (t *imEventTracker) editLoop() {
	// Wait for thinkingMsgID before processing any edits.
	select {
	case <-t.msgIDReady:
	case <-t.ctx.Done():
		return
	}

	for {
		select {
		case text := <-t.editCh:
			// Drain to latest (multiple onEvent calls may have queued during rate-limit wait)
			text = drainLatest(t.editCh, text)
			if t.thinkingMsgID != "" {
				if err := t.p.EditMessage(t.ctx, t.thinkingMsgID, text); err != nil {
					slog.Debug("status edit failed", "msg_id", t.thinkingMsgID, "err", err)
				}
			}
			// Rate limit: wait 1s before next edit
			select {
			case <-time.After(time.Second):
			case <-t.ctx.Done():
				return
			}
		case <-t.ctx.Done():
			return
		}
	}
}

// drainLatest returns the most recent value from ch, or fallback if ch is empty.
func drainLatest(ch chan string, fallback string) string {
	latest := fallback
	for {
		select {
		case v := <-ch:
			latest = v
		default:
			return latest
		}
	}
}

func (t *imEventTracker) waitReady() {
	// Ensure the channel is closed even if onEvent was never called
	// (e.g. Claude returned a result with no streaming assistant events).
	t.sent.Do(func() {
		close(t.msgIDReady)
	})
	<-t.msgIDReady
}
