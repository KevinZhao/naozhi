package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// Server is the HTTP entry point for Naozhi.
type Server struct {
	addr      string
	mux       *http.ServeMux
	platforms map[string]platform.Platform
	router    *session.Router
	dedup     *platform.Dedup
	startedAt time.Time
}

// New creates a new Server.
func New(addr string, router *session.Router, platforms map[string]platform.Platform) *Server {
	return &Server{
		addr:      addr,
		mux:       http.NewServeMux(),
		platforms: platforms,
		router:    router,
		dedup:     platform.NewDedup(10000),
		startedAt: time.Now(),
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

	srv := &http.Server{Addr: s.addr, Handler: s.mux}
	go func() {
		<-ctx.Done()
		slog.Info("shutting down server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	return srv.ListenAndServe()
}

func (s *Server) buildMessageHandler() platform.MessageHandler {
	return func(ctx context.Context, msg platform.IncomingMessage) {
		if s.dedup.Seen(msg.EventID) {
			return
		}

		log := slog.With("platform", msg.Platform, "user", msg.UserID, "chat", msg.ChatID)

		// Check for /new reset command
		if strings.TrimSpace(msg.Text) == "/new" {
			key := session.SessionKey(msg.Platform, msg.ChatType, msg.ChatID, "general")
			s.router.Reset(key)
			p := s.platforms[msg.Platform]
			if p != nil {
				p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "对话已重置。"})
			}
			log.Info("session reset by user")
			return
		}

		key := session.SessionKey(msg.Platform, msg.ChatType, msg.ChatID, "general")

		sess, err := s.router.GetOrCreate(ctx, key)
		if err != nil {
			log.Error("get session", "err", err)
			p := s.platforms[msg.Platform]
			if p != nil {
				p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "系统繁忙，请稍后重试。"})
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

		log.Info("message received", "text_len", len(msg.Text))

		result, err := sess.Send(ctx, msg.Text, onEvent)
		if err != nil {
			log.Error("send to claude", "err", err)
			p.Reply(ctx, platform.OutgoingMessage{ChatID: msg.ChatID, Text: "处理失败，请重试。"})
			return
		}

		log.Info("message replied", "result_len", len(result.Text), "cost", result.CostUSD)

		// Edit "thinking..." to final result, or send new message
		replyText := result.Text
		if thinkingMsgID != "" {
			if err := p.EditMessage(ctx, thinkingMsgID, replyText); err != nil {
				slog.Warn("edit message failed, sending new", "err", err)
				s.sendSplitReply(ctx, p, msg.ChatID, replyText)
			}
		} else {
			s.sendSplitReply(ctx, p, msg.ChatID, replyText)
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
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "ok",
		"uptime":   time.Since(s.startedAt).Round(time.Second).String(),
		"sessions": map[string]int{"active": active, "total": total},
	})
}
