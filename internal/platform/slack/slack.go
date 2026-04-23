package slack

import (
	"bytes"
	"context"
	"errors"
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
	cfg       Config
	api       *slack.Client
	handler   platform.MessageHandler
	cancel    context.CancelFunc
	ctx       context.Context // lifecycle context, cancelled on Stop
	done      chan struct{}
	startMu   sync.Mutex
	started   bool
	botID     string
	handlerWg sync.WaitGroup // tracks in-flight message handler goroutines
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

func (s *Slack) SupportsInterimMessages() bool { return true }

// RegisterRoutes is a no-op for Socket Mode (no inbound HTTP needed).
func (s *Slack) RegisterRoutes(_ *http.ServeMux, _ platform.MessageHandler) {}

// Start implements RunnablePlatform. Launches Socket Mode connection.
func (s *Slack) Start(handler platform.MessageHandler) error {
	// Initialize lifecycle state (ctx/cancel/done) under startMu so a
	// concurrent Stop() that observes `started=true` cannot race a
	// half-initialized ctx. Stop() also acquires startMu now, making
	// Start+Stop mutually exclusive at initialization time.
	s.startMu.Lock()
	if s.started {
		s.startMu.Unlock()
		return fmt.Errorf("slack platform already started")
	}
	s.started = true
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.ctx = ctx
	s.done = make(chan struct{})
	// Assign handler BEFORE releasing startMu so a racing goroutine that
	// observes started==true through a future peek path can only see the
	// fully-initialised Slack value. Leaving it outside the lock used to
	// give other observers a window where s.handler was still nil.
	s.handler = handler
	s.startMu.Unlock()

	// Fetch bot user ID for mention detection
	authResp, err := s.api.AuthTest()
	if err != nil {
		slog.Warn("slack auth test failed — all channel messages will be processed (no mention filtering)", "err", err)
	} else {
		s.botID = authResp.UserID
		slog.Info("slack bot identity", "user_id", s.botID, "team", authResp.Team)
	}

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
	// Snapshot lifecycle handles under startMu so a pre-Start Stop() or a
	// racing Start() cannot hand us a nil ctx/cancel. If the platform was
	// never started, there is nothing to tear down.
	s.startMu.Lock()
	cancel := s.cancel
	done := s.done
	started := s.started
	s.startMu.Unlock()
	if !started || cancel == nil {
		return nil
	}
	cancel()
	<-done
	s.handlerWg.Wait()
	return nil
}

// Reply sends a message to a Slack channel. Handles text and/or images.
func (s *Slack) Reply(ctx context.Context, msg platform.OutgoingMessage) (string, error) {
	// Upload images as file attachments
	for _, img := range msg.Images {
		ext := platform.ImageExt(img.MimeType)
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

// reactionEmojiName maps platform-agnostic ReactionType to a Slack emoji name.
// Empty return means unsupported → caller should skip.
func reactionEmojiName(r platform.ReactionType) string {
	switch r {
	case platform.ReactionQueued:
		return "eyes"
	}
	return ""
}

// parseMsgRef splits our composite "channel:ts" messageID into a slack.ItemRef.
func parseMsgRef(msgID string) (slack.ItemRef, error) {
	parts := strings.SplitN(msgID, ":", 2)
	if len(parts) != 2 {
		return slack.ItemRef{}, fmt.Errorf("invalid slack msgID format: %q", msgID)
	}
	return slack.ItemRef{Channel: parts[0], Timestamp: parts[1]}, nil
}

// AddReaction implements platform.Reactor by calling reactions.add on the
// message identified by "channel:ts". Slack surfaces "already_reacted" as
// an error; treat it as success so retries are idempotent.
func (s *Slack) AddReaction(ctx context.Context, messageID string, r platform.ReactionType) error {
	if messageID == "" {
		return fmt.Errorf("slack AddReaction: empty messageID")
	}
	name := reactionEmojiName(r)
	if name == "" {
		return fmt.Errorf("slack AddReaction: unsupported reaction %q", r)
	}
	ref, err := parseMsgRef(messageID)
	if err != nil {
		return err
	}
	if err := s.api.AddReactionContext(ctx, name, ref); err != nil {
		if isSlackErrCode(err, "already_reacted") {
			return nil
		}
		return fmt.Errorf("slack add reaction: %w", err)
	}
	return nil
}

// RemoveReaction implements platform.Reactor. "no_reaction" (not present)
// is treated as success so callers don't need to track whether a prior Add
// actually landed.
func (s *Slack) RemoveReaction(ctx context.Context, messageID string, r platform.ReactionType) error {
	if messageID == "" {
		return nil
	}
	name := reactionEmojiName(r)
	if name == "" {
		return nil
	}
	ref, err := parseMsgRef(messageID)
	if err != nil {
		return err
	}
	if err := s.api.RemoveReactionContext(ctx, name, ref); err != nil {
		if isSlackErrCode(err, "no_reaction") {
			return nil
		}
		return fmt.Errorf("slack remove reaction: %w", err)
	}
	return nil
}

// isSlackErrCode unwraps Slack's typed error response and returns true if the
// code matches. Falls back to substring match on the unwrapped message — some
// slack-go transport errors are plain errors.errorString, not SlackErrorResponse.
func isSlackErrCode(err error, code string) bool {
	var resp slack.SlackErrorResponse
	if errors.As(err, &resp) {
		return resp.Err == code
	}
	return strings.Contains(err.Error(), code)
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

	// Slack ChannelType values: "im" (1:1 DM), "mpim" (multi-party DM),
	// "channel" (public), "group" (private channel). Multi-party DMs must
	// map to "group" so each mpim gets its own session key; otherwise every
	// participant of every mpim collapses into a single "direct" bucket.
	chatType := "direct"
	if ev.ChannelType == "channel" || ev.ChannelType == "group" || ev.ChannelType == "mpim" {
		chatType = "group"
	}

	msg := platform.IncomingMessage{
		Platform:  "slack",
		EventID:   ev.TimeStamp,
		MessageID: ev.Channel + ":" + ev.TimeStamp,
		UserID:    ev.User,
		ChatID:    ev.Channel,
		ChatType:  chatType,
		Text:      text,
		MentionMe: mentionMe,
	}

	s.handlerWg.Add(1)
	go func() {
		defer s.handlerWg.Done()
		defer platform.RecoverHandler("slack")
		s.handler(s.ctx, msg)
	}()
}
