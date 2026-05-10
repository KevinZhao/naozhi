package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
)

// SendQuestionCard posts an AskUserQuestion prompt to the Feishu chat.
//
// Multi-question cards (len(Items) > 1) are rendered as a read-only
// markdown card that enumerates every question + options and asks the
// user to reply with a free-form text containing all answers at once.
// This avoids the Feishu interactive-card pitfall where each button click
// immediately fires a separate card_action event — which would deliver a
// partial answer to CC before the user finished picking the rest.
//
// Single-question cards (len(Items) == 1) keep the per-option buttons
// since one click unambiguously IS the full answer.
//
// Feishu card schema 2.0: open.feishu.cn/document/feishu-cards/quick-start
func (f *Feishu) SendQuestionCard(ctx context.Context, chatID string, card platform.QuestionCard) (string, error) {
	if len(card.Items) == 0 {
		return "", fmt.Errorf("feishu question card: no items")
	}

	var body []byte
	var err error
	if len(card.Items) == 1 {
		body, err = buildQuestionCardJSON(card)
	} else {
		// Multi-question: markdown-only card with typed-reply hint.
		body, err = buildMultiQuestionMarkdownCardJSON(card)
	}
	if err != nil {
		return "", fmt.Errorf("build question card: %w", err)
	}

	token, err := f.getAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("get access token: %w", err)
	}

	reqBody, err := json.Marshal(struct {
		ReceiveID string `json:"receive_id"`
		MsgType   string `json:"msg_type"`
		Content   string `json:"content"`
	}{ReceiveID: chatID, MsgType: "interactive", Content: string(body)})
	if err != nil {
		return "", fmt.Errorf("marshal request body: %w", err)
	}
	return f.postMessage(ctx, token, reqBody)
}

// buildMultiQuestionMarkdownCardJSON renders a read-only card that lists
// every question + numbered options and asks the user to reply in one
// message. We keep the same schema-2.0 envelope so the markdown renders
// with the full GitHub-flavored feature set; the absence of an action
// block is what makes it free-form-answer only.
func buildMultiQuestionMarkdownCardJSON(card platform.QuestionCard) ([]byte, error) {
	var b strings.Builder
	b.WriteString("**Claude 想请你确认以下问题，请在一条消息里一次回复全部：**\n")
	for qi, item := range card.Items {
		b.WriteString("\n")
		if item.Header != "" {
			fmt.Fprintf(&b, "**【%s】** ", escapeMarkdown(item.Header))
		} else {
			fmt.Fprintf(&b, "**问题 %d** ", qi+1)
		}
		b.WriteString(escapeMarkdown(item.Question))
		if item.MultiSelect {
			b.WriteString("  *(可多选)*")
		}
		b.WriteString("\n")
		for _, opt := range item.Options {
			fmt.Fprintf(&b, "  - %s", escapeMarkdown(opt.Label))
			if opt.Description != "" {
				fmt.Fprintf(&b, " — %s", escapeMarkdown(opt.Description))
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\n回复示例：")
	// Build a friendly example using the first option of each question.
	parts := make([]string, 0, len(card.Items))
	for _, item := range card.Items {
		if len(item.Options) == 0 {
			continue
		}
		h := item.Header
		if h == "" {
			h = item.Question
			if len(h) > 20 {
				h = h[:20]
			}
		}
		parts = append(parts, h+"："+item.Options[0].Label)
	}
	if len(parts) > 0 {
		b.WriteString("「")
		b.WriteString(strings.Join(parts, "；"))
		b.WriteString("」")
	}

	payload := map[string]any{
		"schema": "2.0",
		"body": map[string]any{
			"elements": []any{
				map[string]any{"tag": "markdown", "content": b.String()},
			},
		},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// buildQuestionCardJSON constructs a Feishu schema-2.0 interactive card with
// buttons. Wire format (verified against open.feishu.cn schema docs):
//
//	{
//	  "schema": "2.0",
//	  "body": { "elements": [ ... ] }
//	}
//
// Elements rendered per question group:
//   - markdown text for the header + question
//   - an action_module with one button per option, each carrying a `value`
//     object that our card-action webhook handler decodes back to a plain
//     user text reply.
//
// Multi-select: we emit the same single-press buttons — one click =
// one answer. A later iteration can upgrade to the Feishu `multi_select`
// form input for true multi-select semantics.
func buildQuestionCardJSON(card platform.QuestionCard) ([]byte, error) {
	type action struct {
		Tag  string `json:"tag"`
		Text struct {
			Tag     string `json:"tag"`
			Content string `json:"content"`
		} `json:"text"`
		Type  string         `json:"type"`
		Value map[string]any `json:"value"`
	}
	type actionModule struct {
		Tag     string   `json:"tag"`
		Layout  string   `json:"layout,omitempty"`
		Actions []action `json:"actions"`
	}
	type markdownElem struct {
		Tag     string `json:"tag"`
		Content string `json:"content"`
	}

	var elements []any
	// Leading title so the user immediately sees this is an AskUserQuestion
	// card rather than free-form chat output.
	elements = append(elements, markdownElem{Tag: "markdown", Content: "**Claude 想请你确认**"})

	for _, item := range card.Items {
		// Heading + question text. Combine into one markdown block to keep
		// the card compact; an empty header just produces bare question text.
		var b strings.Builder
		if item.Header != "" {
			fmt.Fprintf(&b, "**%s**\n", escapeMarkdown(item.Header))
		}
		b.WriteString(escapeMarkdown(item.Question))
		elements = append(elements, markdownElem{Tag: "markdown", Content: b.String()})

		acts := make([]action, 0, len(item.Options))
		for _, opt := range item.Options {
			btnText := opt.Label
			if opt.Description != "" {
				// Feishu buttons don't have a secondary label so keep it on
				// the main text; markdown isn't supported on button text, so
				// use a simple dash separator.
				btnText = opt.Label + " — " + opt.Description
			}
			// Clip button labels — Feishu truncates anyway, and an overly
			// long label breaks card layout on narrow clients.
			if len(btnText) > 100 {
				btnText = btnText[:100]
			}
			a := action{Tag: "button", Type: "default"}
			a.Text.Tag = "plain_text"
			a.Text.Content = btnText
			a.Value = map[string]any{
				"kind":        "ask_answer",
				"tool_use_id": card.ToolUseID,
				"session_key": card.SessionKey,
				"header":      item.Header,
				"label":       opt.Label,
			}
			acts = append(acts, a)
		}
		elements = append(elements, actionModule{Tag: "action", Layout: "flow", Actions: acts})
	}

	payload := map[string]any{
		"schema": "2.0",
		"body":   map[string]any{"elements": elements},
	}

	// SetEscapeHTML(false) matches buildMarkdownCardJSON — preserve `<`, `>`,
	// `&` verbatim so code snippets in option descriptions render cleanly.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// escapeMarkdown escapes the minimal set of markdown metacharacters that
// could break the card body rendering if they appear literally in a user-
// facing string. Feishu markdown is GitHub-flavoured; we only guard against
// accidental emphasis / code-span triggers — real code blocks from CC
// output are not expected here (headers and questions are short prose).
func escapeMarkdown(s string) string {
	// Keep the set small to avoid over-escaping common punctuation.
	r := strings.NewReplacer(
		"\\", "\\\\",
		"`", "\\`",
		"*", "\\*",
		"_", "\\_",
	)
	return r.Replace(s)
}

// cardActionPayload is the value object our SendQuestionCard emits and the
// webhook handler expects. Kept package-private; dispatcher never reaches in.
type cardActionPayload struct {
	Kind       string `json:"kind"`
	ToolUseID  string `json:"tool_use_id"`
	SessionKey string `json:"session_key"`
	Header     string `json:"header"`
	Label      string `json:"label"`
}

// handleCardActionWebhook parses an im.card.action.v1_trigger envelope and
// re-enters the normal MessageHandler path with a synthesised text message.
// The caller is the webhook HTTP handler, which already validated token +
// signature + replay.
//
// We tolerate two observed event shapes:
//
//  1. v1 card action: {"action":{"value":{...}},"open_chat_id":"oc_...",
//     "open_message_id":"om_...","operator":{"open_id":"ou_..."}}
//  2. v2 card action: {"event":{"action":{"value":{...}},...}} — the
//     envelope's top-level Event is already the inner object.
//
// Both land on the same payload shape once `action.value` is pulled out.
func (f *Feishu) handleCardActionWebhook(ctx context.Context, raw json.RawMessage, handler platform.MessageHandler) {
	var outer struct {
		Action struct {
			Value cardActionPayload `json:"value"`
		} `json:"action"`
		OpenChatID    string `json:"open_chat_id"`
		OpenMessageID string `json:"open_message_id"`
		ChatType      string `json:"chat_type"`
		Operator      struct {
			OpenID string `json:"open_id"`
		} `json:"operator"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		slog.Warn("feishu card_action: parse failed", "err", err)
		return
	}
	f.dispatchCardAction(ctx, outer.Action.Value, outer.OpenChatID, outer.OpenMessageID,
		outer.ChatType, outer.Operator.OpenID, handler)
}

// dispatchCardAction turns a validated card click into a synthesised
// MessageHandler call. Shared between webhook + WebSocket card paths so both
// transports produce identical IncomingMessage shapes.
func (f *Feishu) dispatchCardAction(
	ctx context.Context,
	val cardActionPayload,
	chatID, messageID, chatType, operatorID string,
	handler platform.MessageHandler,
) {
	if val.Kind != "ask_answer" {
		slog.Debug("feishu card_action: unknown kind, ignoring",
			"kind", osutil.SanitizeForLog(val.Kind, 32))
		return
	}
	text := composeAskAnswerText(val)
	if text == "" {
		slog.Warn("feishu card_action: empty answer text",
			"tool_use_id", osutil.SanitizeForLog(val.ToolUseID, 64))
		return
	}
	ct := "direct"
	if chatType == "group" {
		ct = "group"
	}
	msg := platform.IncomingMessage{
		Platform:  "feishu",
		EventID:   "",
		MessageID: messageID,
		UserID:    operatorID,
		ChatID:    chatID,
		ChatType:  ct,
		Text:      text,
		// Card click on a question targeted to this bot should always route
		// through dispatch regardless of group mention_only gating — the
		// user explicitly interacted with the bot's card.
		MentionMe: true,
	}
	// Best-effort: edit the original card so the chosen option is visible
	// and buttons go away. Failures don't stop dispatching the answer —
	// the conversation continuation matters more than UI polish.
	if messageID != "" && f.cfg.AppID != "" {
		go func() {
			defer func() {
				// A mid-goroutine panic (e.g. uninitialised client in tests)
				// would otherwise crash the process; best-effort semantics
				// say swallow quietly.
				if r := recover(); r != nil {
					slog.Debug("feishu card_action: edit panic recovered",
						"msg_id", osutil.SanitizeForLog(messageID, 64), "panic", r)
				}
			}()
			if err := f.EditMessage(ctx, messageID, "✅ 已回答：**"+escapeMarkdown(text)+"**"); err != nil {
				slog.Debug("feishu card_action: edit original card failed",
					"msg_id", osutil.SanitizeForLog(messageID, 64), "err", err)
			}
		}()
	}
	handler(ctx, msg)
}

// composeAskAnswerText turns a card-click payload into the plain user text
// that gets injected as a new user message. Mirrors the dashboard's
// composeAskAnswer format ("Header: Label.") so CC sees a stable reply shape
// regardless of which surface the user answered from.
func composeAskAnswerText(p cardActionPayload) string {
	h := strings.TrimSpace(p.Header)
	l := strings.TrimSpace(p.Label)
	if l == "" {
		return ""
	}
	if h == "" {
		return l + "."
	}
	return h + ": " + l + "."
}
