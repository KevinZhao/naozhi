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
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
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
	ctx     context.Context // lifecycle context, cancelled on Stop
	done    chan struct{}
	startMu sync.Mutex
	started bool
	// botID is the bot's own user ID for @-mention detection; written by
	// Start() and the self-heal, read by handleMessage goroutines (botIDMu).
	// Empty means AuthTest has not yet succeeded.
	botIDMu sync.RWMutex
	botID   string
	// botHealAt is the next time the AuthTest self-heal may run; guarded by botIDMu.
	botHealAt time.Time
	// dispatch bounds concurrent handler goroutines (a noisy workspace could
	// otherwise OOM naozhi) and lets Stop() drain them.
	dispatch platform.BoundedDispatch
}

// slackHTTPClient is shared by all Slack adapters. The 10s Timeout matters
// because slack-go treats ctx cancellation as advisory and a slow response
// would pin goroutines past Stop()'s drain. CheckRedirect blocks all 3xx so a
// MITM'd path cannot redirect the bearer-token request to an internal address.
var slackHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// New creates a Slack platform adapter.
func New(cfg Config) *Slack {
	if cfg.MaxReplyLen <= 0 {
		cfg.MaxReplyLen = platform.DefaultMaxReplyLen
	}
	api := slack.New(
		cfg.BotToken,
		slack.OptionAppLevelToken(cfg.AppToken),
		slack.OptionHTTPClient(slackHTTPClient),
	)
	return &Slack{
		cfg:      cfg,
		api:      api,
		dispatch: platform.BoundedDispatch{Name: "slack"},
	}
}

func (s *Slack) Name() string { return "slack" }

func (s *Slack) MaxReplyLength() int { return s.cfg.MaxReplyLen }

func (s *Slack) SupportsInterimMessages() bool { return true }

// RegisterRoutes is a no-op for Socket Mode (no inbound HTTP needed).
func (s *Slack) RegisterRoutes(_ *http.ServeMux, _ platform.MessageHandler) {}

// Start implements RunnablePlatform. Launches Socket Mode connection.
func (s *Slack) Start(handler platform.MessageHandler) error {
	// Defence-in-depth with config.validateConfig: empty credentials would
	// otherwise surface only as an opaque AuthTest error after a network call.
	if s.cfg.BotToken == "" {
		return fmt.Errorf("slack: BotToken required (got empty)")
	}
	if s.cfg.AppToken == "" {
		return fmt.Errorf("slack: AppToken required for Socket Mode (got empty)")
	}
	// Lifecycle state is initialised under startMu so a concurrent Stop()
	// never sees a half-initialised ctx.
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
	s.handler = handler
	s.startMu.Unlock()

	authResp, err := s.api.AuthTest()
	if err != nil {
		slog.Warn("slack auth test failed — all channel messages will be processed (no mention filtering)", "err", err)
	} else {
		s.botIDMu.Lock()
		s.botID = authResp.UserID
		s.botIDMu.Unlock()
		slog.Info("slack bot identity",
			"user_id", authResp.UserID,
			"team", osutil.SanitizeForLog(authResp.Team, 128))
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
	// Snapshot under startMu so a pre-Start or racing Stop() never sees a nil cancel.
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
	s.dispatch.Wait()
	return nil
}

// Reply sends a message to a Slack channel. Handles text and/or images.
func (s *Slack) Reply(ctx context.Context, msg platform.OutgoingMessage) (string, error) {
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
	return platform.EncodeMessageRef(msg.ChatID, ts), nil
}

// EditMessage updates an existing Slack message.
func (s *Slack) EditMessage(ctx context.Context, msgID string, text string) error {
	channel, ts, ok := platform.DecodeMessageRef(msgID)
	if !ok {
		return fmt.Errorf("invalid slack msgID format: %q", msgID)
	}
	_, _, _, err := s.api.UpdateMessageContext(ctx, channel, ts,
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
	channel, ts, ok := platform.DecodeMessageRef(msgID)
	if !ok {
		return slack.ItemRef{}, fmt.Errorf("invalid slack msgID format: %q", msgID)
	}
	return slack.ItemRef{Channel: channel, Timestamp: ts}, nil
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

// slackBotHealCooldown rate-limits the AuthTest self-heal while botID is unknown (#1947).
const slackBotHealCooldown = time.Minute

// botIDOrEmpty returns the cached bot user ID under the read lock. Empty means
// AuthTest has not yet succeeded.
func (s *Slack) botIDOrEmpty() string {
	s.botIDMu.RLock()
	defer s.botIDMu.RUnlock()
	return s.botID
}

// maybeHealBotID kicks one rate-limited background AuthTest while botID is
// unknown so exact-mention filtering recovers after a transient Start failure (#1947).
func (s *Slack) maybeHealBotID() {
	s.botIDMu.Lock()
	if s.botID != "" || time.Now().Before(s.botHealAt) {
		s.botIDMu.Unlock()
		return
	}
	s.botHealAt = time.Now().Add(slackBotHealCooldown)
	s.botIDMu.Unlock()

	s.dispatch.Go("slack bot heal", func() {
		authResp, err := s.api.AuthTest()
		if err != nil {
			slog.Warn("slack auth test self-heal failed; staying fail-open", "err", err)
			return
		}
		s.botIDMu.Lock()
		s.botID = authResp.UserID
		s.botIDMu.Unlock()
		slog.Info("slack bot identity recovered",
			"user_id", authResp.UserID,
			"team", osutil.SanitizeForLog(authResp.Team, 128))
	})
}

func (s *Slack) handleMessage(ev *slackevents.MessageEvent) {
	if ev.BotID != "" || ev.SubType != "" {
		return
	}

	text := ev.Text
	mentionMe := false

	botID := s.botIDOrEmpty()
	if botID != "" {
		mention := "<@" + botID + ">"
		if strings.Contains(text, mention) {
			text = strings.ReplaceAll(text, mention, "")
			mentionMe = true
		}
	} else {
		// botID unknown: mentionMe=false would fail CLOSED (dispatch's group
		// gate drops every group message until restart). Fail open, as the
		// Start-time warn promises, and self-heal (#1947).
		mentionMe = true
		s.maybeHealBotID()
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	// API-posted messages can exceed Slack's UX limit; cap before dispatch.
	const maxSlackInboundBytes = platform.DefaultMaxIncomingBytes
	if len(text) > maxSlackInboundBytes {
		slog.Warn("slack message exceeds inbound text cap, dropping",
			"len", len(text), "channel", ev.Channel)
		return
	}

	// mpim (multi-party DM) must map to "group" so each gets its own session
	// key instead of collapsing into one "direct" bucket.
	chatType := "direct"
	if ev.ChannelType == "channel" || ev.ChannelType == "group" || ev.ChannelType == "mpim" {
		chatType = "group"
	}

	// A bare ts is NOT unique across channels (the fraction is a per-channel
	// sequence); the composite keeps cross-channel messages from deduping (#2015).
	eventID := ev.Channel + ":" + ev.TimeStamp

	msg := platform.IncomingMessage{
		Platform:  "slack",
		EventID:   eventID,
		MessageID: ev.Channel + ":" + ev.TimeStamp,
		UserID:    ev.User,
		ChatID:    ev.Channel,
		ChatType:  chatType,
		Text:      text,
		MentionMe: mentionMe,
	}

	s.dispatch.TryGo("slack", func() { s.handler(s.ctx, msg) },
		"chat", msg.ChatID, "user", msg.UserID)
}
