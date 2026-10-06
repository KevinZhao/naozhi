package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/textutil"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackutilsx"
)

// askActionIDPrefix marks naozhi's AskUserQuestion buttons; a block_actions
// click on any other action_id is not ours to answer.
const askActionIDPrefix = "naozhi_ask_"

// Block Kit limits a question card must respect; past them chat.postMessage
// rejects the whole message, so SendQuestionCard errors and dispatch falls
// back to the plain-text list instead.
const (
	slackButtonTextMaxRunes  = 75
	slackButtonValueMaxBytes = 2000
	slackActionsMaxElements  = 25
	slackTextObjectMaxRunes  = 3000
)

// SendQuestionCard posts an AskUserQuestion prompt as Block Kit. A single
// question gets one button per option (one click is the full answer); several
// questions get a read-only list asking for one text reply, because each click
// would deliver a partial answer.
func (s *Slack) SendQuestionCard(ctx context.Context, chatID string, card platform.QuestionCard) (string, error) {
	blocks, text, err := buildQuestionBlocks(card)
	if err != nil {
		return "", fmt.Errorf("slack question card: %w", err)
	}
	opts := []slack.MsgOption{slack.MsgOptionText(text, true), slack.MsgOptionBlocks(blocks...)}
	if card.ThreadID != "" {
		opts = append(opts, slack.MsgOptionTS(card.ThreadID))
	}
	_, ts, _, err := s.api.SendMessageContext(ctx, chatID, opts...)
	if err != nil {
		return "", fmt.Errorf("slack send question card: %w", err)
	}
	return platform.EncodeMessageRef(chatID, ts), nil
}

// buildQuestionBlocks renders card as Block Kit plus the plain notification
// text Slack shows where blocks are not rendered.
func buildQuestionBlocks(card platform.QuestionCard) ([]slack.Block, string, error) {
	if len(card.Items) == 0 {
		return nil, "", errors.New("no items")
	}
	text := "Claude 想请你确认：\n" + platform.RenderAskQuestionPlain(card.Items)
	if len(card.Items) > 1 {
		body := "*Claude 想请你确认以下问题，请在一条消息里一次回复全部：*\n" +
			slackutilsx.EscapeMessage(platform.RenderAskQuestionPlain(card.Items))
		if utf8.RuneCountInString(body) > slackTextObjectMaxRunes {
			return nil, "", errors.New("questions exceed the section text limit")
		}
		return []slack.Block{mrkdwnSection(body)}, text, nil
	}

	item := card.Items[0]
	if len(item.Options) > slackActionsMaxElements {
		return nil, "", fmt.Errorf("%d options exceed the %d-button limit", len(item.Options), slackActionsMaxElements)
	}
	var b strings.Builder
	b.WriteString("*Claude 想请你确认*\n")
	if item.Header != "" {
		b.WriteString("*" + slackutilsx.EscapeMessage(item.Header) + "*\n")
	}
	b.WriteString(slackutilsx.EscapeMessage(item.Question))
	if utf8.RuneCountInString(b.String()) > slackTextObjectMaxRunes {
		return nil, "", errors.New("question exceeds the section text limit")
	}
	blocks := []slack.Block{mrkdwnSection(b.String())}

	var desc []string
	buttons := make([]slack.BlockElement, 0, len(item.Options))
	for i, opt := range item.Options {
		if opt.Description != "" {
			desc = append(desc, "• *"+slackutilsx.EscapeMessage(opt.Label)+"* — "+slackutilsx.EscapeMessage(opt.Description))
		}
		value, err := marshalAskValue(platform.NewAskAnswerPayload(card, item, opt))
		if err != nil {
			return nil, "", err
		}
		if len(value) > slackButtonValueMaxBytes {
			return nil, "", fmt.Errorf("option %d value is %d bytes, over the %d-byte limit", i, len(value), slackButtonValueMaxBytes)
		}
		label := textutil.TruncateRunesNoEllipsis(opt.Label, slackButtonTextMaxRunes)
		buttons = append(buttons, slack.NewButtonBlockElement(askActionIDPrefix+strconv.Itoa(i), value,
			slack.NewTextBlockObject(slack.PlainTextType, label, false, false)))
	}
	if len(desc) > 0 {
		// Descriptions only explain the options, so clipping them is safe.
		blocks = append(blocks, mrkdwnContext(textutil.TruncateRunes(strings.Join(desc, "\n"), slackTextObjectMaxRunes-3)))
	}
	if len(buttons) > 0 {
		blocks = append(blocks, slack.NewActionBlock("", buttons...))
	}
	// The hint keeps the card usable when the app has Interactivity off and
	// the buttons do nothing.
	hint := "也可以直接回复文字作答。"
	if item.MultiSelect {
		hint = "可多选：每个按钮只提交一项，选多项请直接回复文字。"
	}
	blocks = append(blocks, mrkdwnContext(hint))
	return blocks, text, nil
}

// marshalAskValue skips HTML escaping, which would grow each "<", ">" or "&"
// in a label to six bytes of the 2000-byte value budget.
func marshalAskValue(p platform.AskAnswerPayload) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

func mrkdwnSection(text string) *slack.SectionBlock {
	return slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil)
}

func mrkdwnContext(text string) *slack.ContextBlock {
	return slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, text, false, false))
}

// handleBlockActions answers an AskUserQuestion button click: the decoded
// value becomes a synthetic text message pinned to the asking agent, and the
// card's buttons are replaced by the chosen answer.
func (s *Slack) handleBlockActions(cb slack.InteractionCallback) {
	var act *slack.BlockAction
	for _, a := range cb.ActionCallback.BlockActions {
		if a != nil && strings.HasPrefix(a.ActionID, askActionIDPrefix) {
			act = a
			break
		}
	}
	if act == nil {
		return
	}
	var val platform.AskAnswerPayload
	if err := json.Unmarshal([]byte(act.Value), &val); err != nil {
		slog.Debug("slack block_actions: undecodable value", "err", err)
		return
	}
	if val.Kind != platform.AskAnswerKind {
		slog.Debug("slack block_actions: unknown kind, ignoring",
			"kind", osutil.SanitizeForLog(val.Kind, 32))
		return
	}
	text := platform.ComposeAskAnswerText(val)
	channel := cb.Channel.ID
	if channel == "" {
		channel = cb.Container.ChannelID
	}
	if text == "" || channel == "" {
		return
	}
	msgTs := cb.Container.MessageTs
	if msgTs == "" {
		msgTs = cb.Message.Timestamp
	}
	// A card asked in a thread sits in it; the answer's reply goes there too.
	// A top-level card with replies under it is its own thread_ts: no thread.
	threadTs := cb.Container.ThreadTs
	if threadTs == "" {
		threadTs = cb.Message.ThreadTimestamp
	}
	if threadTs == msgTs {
		threadTs = ""
	}
	// block_actions carry no channel_type. Fall back to the channel ID: only
	// IMs start with "D", matching handleMessage's im -> direct mapping.
	chatType := platform.NormalizeAskChatType(val.ChatType)
	if chatType == "" {
		chatType = "group"
		if strings.HasPrefix(channel, "D") {
			chatType = "direct"
		}
	}
	// Socket Mode redelivers an unacked envelope; (message, user, tool_use_id)
	// collapses replays and a second click by the same user.
	tu := textutil.TruncateRunesNoEllipsis(val.ToolUseID, 64)
	eventID := "block_action:" + channel + ":" + msgTs + ":" + cb.User.ID + ":" + tu
	msg := platform.IncomingMessage{
		Platform:  "slack",
		EventID:   osutil.SanitizeForLog(eventID, 256),
		MessageID: platform.EncodeMessageRef(channel, msgTs),
		UserID:    cb.User.ID,
		ChatID:    channel,
		ChatType:  chatType,
		ThreadID:  threadTs,
		Text:      text,
		// The dispatcher whitelist-validates AgentID before routing (#2148).
		AgentID: osutil.SanitizeForLog(val.AgentID, platform.AskIDMaxRunes),
		// An explicit click on the bot's card bypasses mention_only gating.
		MentionMe: true,
	}
	// A dropped answer keeps its buttons so the user can click again.
	if !s.dispatch.TryGo("slack", func() { s.handler(s.ctx, msg) },
		"chat", msg.ChatID, "user", msg.UserID) {
		return
	}
	if msgTs != "" {
		s.dispatch.Go("slack ask card edit", func() { s.markQuestionAnswered(channel, msgTs, text) })
	}
}

// markQuestionAnswered replaces the card's blocks so its buttons disappear.
// EditMessage cannot do this: it sends text only and chat.update keeps the
// existing blocks. Best effort; the answer is already on its way.
func (s *Slack) markQuestionAnswered(channel, ts, answer string) {
	// Detached: the click is acked and nothing else bounds this call.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := "✅ 已回答：" + osutil.SanitizeForLog(answer, 1024)
	_, _, _, err := s.api.UpdateMessageContext(ctx, channel, ts,
		slack.MsgOptionText(done, true),
		slack.MsgOptionBlocks(mrkdwnSection(slackutilsx.EscapeMessage(done))))
	if err != nil {
		slog.Debug("slack block_actions: edit question card failed",
			"channel", channel, "err", err)
	}
}
