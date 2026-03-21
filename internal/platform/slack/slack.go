package slack

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/naozhi/naozhi/internal/platform"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// Config holds Slack app credentials.
type Config struct {
	BotToken    string
	AppToken    string // xapp- token for Socket Mode
	MaxReplyLen int
}

// Slack implements Platform and RunnablePlatform via Socket Mode.
type Slack struct {
	cfg     Config
	api     *slack.Client
	handler platform.MessageHandler
	cancel  context.CancelFunc
	done    chan struct{}
	startMu sync.Mutex
	started bool
	botID   string
}

// New creates a Slack platform adapter.
func New(cfg Config) *Slack {
	if cfg.MaxReplyLen <= 0 {
		cfg.MaxReplyLen = 4000
	}
	api := slack.New(cfg.BotToken, slack.OptionAppLevelToken(cfg.AppToken))
	return &Slack{cfg: cfg, api: api}
}

func (s *Slack) Name() string { return "slack" }

func (s *Slack) MaxReplyLength() int { return s.cfg.MaxReplyLen }

// RegisterRoutes is a no-op for Socket Mode (no inbound HTTP needed).
func (s *Slack) RegisterRoutes(_ *http.ServeMux, _ platform.MessageHandler) {}

// Start implements RunnablePlatform. Launches Socket Mode connection.
func (s *Slack) Start(handler platform.MessageHandler) error {
	s.startMu.Lock()
	if s.started {
		s.startMu.Unlock()
		return fmt.Errorf("slack platform already started")
	}
	s.started = true
	s.startMu.Unlock()

	s.handler = handler

	// Fetch bot user ID for mention detection
	authResp, err := s.api.AuthTest()
	if err != nil {
		slog.Warn("slack auth test failed — all channel messages will be processed (no mention filtering)", "err", err)
	} else {
		s.botID = authResp.UserID
		slog.Info("slack bot identity", "user_id", s.botID, "team", authResp.Team)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})

	client := socketmode.New(s.api)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		slog.Info("slack socket mode starting")
		s.eventLoop(ctx, client)
		slog.Info("slack socket mode stopped")
	}()

	go func() {
		defer wg.Done()
		if err := client.RunContext(ctx); err != nil && ctx.Err() == nil {
			slog.Error("slack socket mode error", "err", err)
		}
	}()

	go func() {
		wg.Wait()
		close(s.done)
	}()

	return nil
}

// Stop implements RunnablePlatform.
func (s *Slack) Stop() error {
	if s.cancel != nil {
		s.cancel()
		<-s.done
	}
	return nil
}

// Reply sends a message to a Slack channel. Handles text and/or images.
func (s *Slack) Reply(ctx context.Context, msg platform.OutgoingMessage) (string, error) {
	// Upload images as file attachments
	for _, img := range msg.Images {
		ext := ".png"
		switch img.MimeType {
		case "image/jpeg":
			ext = ".jpg"
		case "image/gif":
			ext = ".gif"
		}
		_, err := s.api.UploadFileContext(ctx, slack.UploadFileParameters{
			Channel:  msg.ChatID,
			Filename: "image" + ext,
			FileSize: len(img.Data),
			Reader:   bytes.NewReader(img.Data),
		})
		if err != nil {
			slog.Warn("slack upload image failed", "err", err)
		}
	}

	// Send text if present
	if msg.Text == "" {
		return "", nil
	}

	opts := []slack.MsgOption{
		slack.MsgOptionText(msg.Text, false),
	}
	if msg.ThreadID != "" {
		opts = append(opts, slack.MsgOptionTS(msg.ThreadID))
	}
	_, ts, _, err := s.api.SendMessageContext(ctx, msg.ChatID, opts...)
	if err != nil {
		return "", fmt.Errorf("slack send: %w", err)
	}
	return msg.ChatID + ":" + ts, nil
}

// EditMessage updates an existing Slack message.
func (s *Slack) EditMessage(ctx context.Context, msgID string, text string) error {
	parts := strings.SplitN(msgID, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid slack msgID format: %q", msgID)
	}
	_, _, _, err := s.api.UpdateMessageContext(ctx, parts[0], parts[1],
		slack.MsgOptionText(text, false))
	if err != nil {
		return fmt.Errorf("slack edit message: %w", err)
	}
	return nil
}

func (s *Slack) eventLoop(ctx context.Context, client *socketmode.Client) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-client.Events:
			if !ok {
				return
			}
			s.handleSocketEvent(ctx, client, evt)
		}
	}
}

func (s *Slack) handleSocketEvent(_ context.Context, client *socketmode.Client, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeEventsAPI:
		eventsAPI, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		client.Ack(*evt.Request)

		switch ev := eventsAPI.InnerEvent.Data.(type) {
		case *slackevents.MessageEvent:
			s.handleMessage(ev)
		}
	}
}

func (s *Slack) handleMessage(ev *slackevents.MessageEvent) {
	if ev.BotID != "" || ev.SubType != "" {
		return
	}

	text := ev.Text
	mentionMe := false

	if s.botID != "" {
		mention := "<@" + s.botID + ">"
		if strings.Contains(text, mention) {
			text = strings.ReplaceAll(text, mention, "")
			mentionMe = true
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	chatType := "direct"
	if ev.ChannelType == "channel" || ev.ChannelType == "group" {
		chatType = "group"
	}

	msg := platform.IncomingMessage{
		Platform:  "slack",
		EventID:   ev.TimeStamp,
		UserID:    ev.User,
		ChatID:    ev.Channel,
		ChatType:  chatType,
		Text:      text,
		MentionMe: mentionMe,
	}

	go s.handler(context.Background(), msg)
}
