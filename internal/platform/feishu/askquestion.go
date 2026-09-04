package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Rune caps for card-action value fields: they round-trip through the button
// `value` object, and without caps a crafted relay could stuff ~60 KB into CC
// stdin (the webhook body limit is 64 KiB with no per-field cap).
const (
	cardValueLabelMaxRunes  = 512 // labels may include descriptions
	cardValueHeaderMaxRunes = 128 // short prose
	cardValueIDMaxRunes     = 128 // tool_use_id etc
	cardButtonTextMaxRunes  = 100 // Feishu truncates anyway; stay safe
)

// SendQuestionCard posts an AskUserQuestion prompt. Single-question cards
// get per-option buttons (one click IS the full answer); multi-question cards
// are read-only markdown asking for one free-form reply, because each button
// click fires a separate card_action and would deliver a partial answer.
func (f *Feishu) SendQuestionCard(ctx context.Context, chatID string, card platform.QuestionCard) (string, error) {
	if len(card.Items) == 0 {
		return "", fmt.Errorf("feishu question card: no items")
	}

	var body []byte
	var err error
	if len(card.Items) == 1 {
		body, err = buildQuestionCardJSON(card)
	} else {
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

// buildMultiQuestionMarkdownCardJSON renders a read-only card listing every
// question + options and asking for one reply; no action block = free-form only.
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
	parts := make([]string, 0, len(card.Items))
	for _, item := range card.Items {
		if len(item.Options) == 0 {
			continue
		}
		h := item.Header
		if h == "" {
			// Rune-aware: byte-slicing CJK would emit invalid UTF-8 and fail Encode.
			h = textutil.TruncateRunesNoEllipsis(item.Question, 20)
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

// buildQuestionCardJSON constructs a schema-2.0 interactive card: markdown
// header + question, then one button per option whose `value` object the
// card-action handler decodes back into a plain user reply. Multi-select still
// emits single-press buttons (one click = one answer).
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
	elements = append(elements, markdownElem{Tag: "markdown", Content: "**Claude 想请你确认**"})

	for _, item := range card.Items {
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
				// Buttons have no secondary label and no markdown; plain dash separator.
				btnText = opt.Label + " — " + opt.Description
			}
			// Rune-boundary clip: byte-level slicing could produce invalid UTF-8.
			btnText = textutil.TruncateRunesNoEllipsis(btnText, cardButtonTextMaxRunes)
			a := action{Tag: "button", Type: "default"}
			a.Text.Tag = "plain_text"
			a.Text.Content = btnText
			// No session_key: routing re-derives from chat context, and every
			// value is rune-capped so a replay cannot bounce 60 KB into CC stdin.
			a.Value = map[string]any{
				"kind":        "ask_answer",
				"tool_use_id": textutil.TruncateRunesNoEllipsis(card.ToolUseID, cardValueIDMaxRunes),
				"header":      textutil.TruncateRunesNoEllipsis(item.Header, cardValueHeaderMaxRunes),
				"label":       textutil.TruncateRunesNoEllipsis(opt.Label, cardValueLabelMaxRunes),
			}
			// agent_id routes the answer back to the asking agent session; the
			// callback has no agent dimension and the dispatcher whitelist-
			// validates the value before honouring it (#2148).
			if card.AgentID != "" {
				a.Value["agent_id"] = textutil.TruncateRunesNoEllipsis(card.AgentID, cardValueIDMaxRunes)
			}
			// chat_type lets the WebSocket card path (whose callback lacks it)
			// route back to the same session key; only whitelisted values are emitted.
			if ct := normalizeCardChatType(card.ChatType); ct != "" {
				a.Value["chat_type"] = ct
			}
			acts = append(acts, a)
		}
		elements = append(elements, actionModule{Tag: "action", Layout: "flow", Actions: acts})
	}

	payload := map[string]any{
		"schema": "2.0",
		"body":   map[string]any{"elements": elements},
	}

	// No HTML escaping so code snippets in option descriptions render cleanly.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// markdownEscaper is shared: NewReplacer builds a trie per call and a
// multi-question card escapes O(questions × options) strings.
var markdownEscaper = strings.NewReplacer(
	"\\", "\\\\",
	"`", "\\`",
	"*", "\\*",
	"_", "\\_",
)

// normalizeCardChatType whitelists to {"direct","group"}; anything else returns
// "" so an attacker-relayed value never reaches the session key.
func normalizeCardChatType(s string) string {
	switch s {
	case "direct", "group":
		return s
	default:
		return ""
	}
}

// escapeMarkdown escapes the emphasis / code-span metacharacters that could
// break card rendering in short prose (headers, questions).
func escapeMarkdown(s string) string {
	return markdownEscaper.Replace(s)
}

// cardActionPayload is the button `value` object SendQuestionCard emits and
// the card-action handlers decode. Deliberately minimal: session routing is
// re-derived from the click's chat context, never from embedded state.
type cardActionPayload struct {
	Kind      string `json:"kind"`
	ToolUseID string `json:"tool_use_id"`
	Header    string `json:"header"`
	Label     string `json:"label"`
	// ChatType ("direct"/"group") is whitelisted on read and consulted only by
	// the WebSocket path, whose callback lacks chat_type; the webhook path
	// reads the signed envelope instead. Needed because p2p chats also use an
	// "oc_" open_chat_id, so a prefix heuristic mis-routes 1:1 answers.
	ChatType string `json:"chat_type,omitempty"`
	// AgentID routes the answer back to the asking agent session; the
	// dispatcher whitelist-validates it against the known agent set before it
	// can influence routing (#2148).
	AgentID string `json:"agent_id,omitempty"`
}

// handleCardActionWebhook parses an im.card.action.v1_trigger envelope (token
// + signature + replay already validated by the caller) and re-enters the
// MessageHandler path with a synthesised text message. Both the v1 shape
// (ids at top level) and the v2 shape (ids nested under `context`) are
// accepted; without the fallback v2 ids decode to "" (#2006).
func (f *Feishu) handleCardActionWebhook(ctx context.Context, raw json.RawMessage, handler platform.MessageHandler) {
	var outer struct {
		Action struct {
			Value cardActionPayload `json:"value"`
		} `json:"action"`
		OpenChatID    string `json:"open_chat_id"`
		OpenMessageID string `json:"open_message_id"`
		ChatType      string `json:"chat_type"`
		Context       struct {
			OpenChatID    string `json:"open_chat_id"`
			OpenMessageID string `json:"open_message_id"`
		} `json:"context"`
		Operator struct {
			OpenID string `json:"open_id"`
		} `json:"operator"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		slog.Warn("feishu card_action: parse failed", "err", err)
		return
	}
	chatID := outer.OpenChatID
	if chatID == "" {
		chatID = outer.Context.OpenChatID
	}
	messageID := outer.OpenMessageID
	if messageID == "" {
		messageID = outer.Context.OpenMessageID
	}
	f.dispatchCardAction(ctx, outer.Action.Value, chatID, messageID,
		outer.ChatType, outer.Operator.OpenID, handler)
}

// dispatchCardAction turns a validated card click into a synthesised
// MessageHandler call; shared by the webhook and WebSocket paths.
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
	// The v2 envelope carries no chat_type at any level; fall back to the
	// button value before defaulting to "direct", or a group answer routes
	// into a phantom direct session (#2007).
	ct := chatType
	if ct == "" {
		ct = normalizeCardChatType(val.ChatType)
	}
	if ct != "group" {
		ct = "direct"
	}
	// Dedup key: the Lark SDK can re-deliver a card_action on WS reconnect.
	// (message, operator, tool_use_id) collapses replays; the prefix avoids
	// collisions with message event IDs in the same Dedup bucket.
	eventID := ""
	if messageID != "" {
		// Cap each component so a long open_id cannot shadow the message ID
		// past the SanitizeForLog truncation.
		op := operatorID
		if len(op) > 64 {
			op = op[:64]
		}
		tu := val.ToolUseID
		if len(tu) > 64 {
			tu = tu[:64]
		}
		eventID = "card_action:" + messageID + ":" + op + ":" + tu
	}
	msg := platform.IncomingMessage{
		Platform:  "feishu",
		EventID:   osutil.SanitizeForLog(eventID, 256),
		MessageID: messageID,
		UserID:    operatorID,
		ChatID:    chatID,
		ChatType:  ct,
		Text:      text,
		// Sanitised here; the dispatcher whitelist-validates before routing (#2148).
		AgentID: osutil.SanitizeForLog(val.AgentID, cardValueIDMaxRunes),
		// The user explicitly clicked the bot's card: bypass mention_only gating.
		MentionMe: true,
	}
	// Best-effort edit of the original card; failures never block the answer.
	if messageID != "" && f.cfg.AppID != "" {
		// escapeMarkdown covers emphasis only, not C1/bidi/LS/PS.
		safeText := osutil.SanitizeForLog(text, 1024)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Debug("feishu card_action: edit panic recovered",
						"msg_id", osutil.SanitizeForLog(messageID, 64), "panic", r)
				}
			}()
			// Detached from the callback ctx, which may be cancelled as soon as
			// the transport's outer handler returns.
			editCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := f.EditMessage(editCtx, messageID, "✅ 已回答：**"+escapeMarkdown(safeText)+"**"); err != nil {
				slog.Debug("feishu card_action: edit original card failed",
					"msg_id", osutil.SanitizeForLog(messageID, 64), "err", err)
			}
		}()
	}
	handler(ctx, msg)
}

// composeAskAnswerText renders a card click as the dashboard's "Header: Label."
// reply shape. Header and label are rune-capped and control-stripped so a
// hostile relay cannot land oversized or bidi-injected strings on CC stdin.
func composeAskAnswerText(p cardActionPayload) string {
	h := strings.TrimSpace(textutil.TruncateRunesNoEllipsis(osutil.SanitizeForLog(p.Header, 0), cardValueHeaderMaxRunes))
	l := strings.TrimSpace(textutil.TruncateRunesNoEllipsis(osutil.SanitizeForLog(p.Label, 0), cardValueLabelMaxRunes))
	if l == "" {
		return ""
	}
	if h == "" {
		return l + "."
	}
	return h + ": " + l + "."
}
