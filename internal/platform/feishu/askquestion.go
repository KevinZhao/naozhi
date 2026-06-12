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

// Length caps for card-action value fields. Labels and headers ride in the
// button `value` object round-trip; without caps, a crafted Feishu relay
// (or a replay of a signed mutated envelope) could stuff 60 KB strings into
// CC stdin — the webhook body limit is 64 KiB and there's no per-field cap.
// Match ~4 KiB worth of runes so card-sourced user messages can't exceed
// the normal IM text budget.
const (
	cardValueLabelMaxRunes  = 512 // labels may include descriptions
	cardValueHeaderMaxRunes = 128 // short prose
	cardValueIDMaxRunes     = 128 // tool_use_id etc
	cardButtonTextMaxRunes  = 100 // Feishu truncates anyway; stay safe
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
			// Rune-aware truncation — byte-slicing [:20] would split multi-byte
			// CJK codepoints and emit invalid UTF-8 into the card body, making
			// json.Encoder.Encode fail and aborting the whole card send.
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
			// Clip button label at rune boundary — byte-level [:100] could
			// split a multi-byte CJK sequence into invalid UTF-8 that
			// Feishu's relay would reject or render as mojibake.
			btnText = textutil.TruncateRunesNoEllipsis(btnText, cardButtonTextMaxRunes)
			a := action{Tag: "button", Type: "default"}
			a.Text.Tag = "plain_text"
			a.Text.Content = btnText
			// session_key is no longer embedded — the inbound path never read
			// it (routing re-derives from chat context) and keeping it
			// widened the attack surface. Values are rune-capped so an
			// inbound replay can't bounce 60 KB into CC stdin.
			a.Value = map[string]any{
				"kind":        "ask_answer",
				"tool_use_id": textutil.TruncateRunesNoEllipsis(card.ToolUseID, cardValueIDMaxRunes),
				"header":      textutil.TruncateRunesNoEllipsis(item.Header, cardValueHeaderMaxRunes),
				"label":       textutil.TruncateRunesNoEllipsis(opt.Label, cardValueLabelMaxRunes),
			}
			// Embed the originating chat type so the WebSocket card-action
			// path (whose callback lacks chat_type and would otherwise infer
			// "group" from the oc_ prefix even for 1:1 chats) routes the
			// answer back to the same session key. Only the whitelisted
			// values are emitted; anything else is omitted and the receiver
			// falls back to its heuristic.
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

// markdownEscaper is reused across every escapeMarkdown call. NewReplacer
// builds an internal trie per invocation; a multi-question card hits
// escapeMarkdown O(questions × options) times and each call would otherwise
// allocate a fresh replacer during the send hot path.
var markdownEscaper = strings.NewReplacer(
	"\\", "\\\\",
	"`", "\\`",
	"*", "\\*",
	"_", "\\_",
)

// normalizeCardChatType whitelists a chat-type string down to the two values
// the session router understands. Anything outside {"direct","group"} returns
// "" so callers fall back to their own heuristic rather than propagating an
// unexpected (possibly attacker-relayed) value into the session key.
func normalizeCardChatType(s string) string {
	switch s {
	case "direct", "group":
		return s
	default:
		return ""
	}
}

// escapeMarkdown escapes the minimal set of markdown metacharacters that
// could break the card body rendering if they appear literally in a user-
// facing string. Feishu markdown is GitHub-flavoured; we only guard against
// accidental emphasis / code-span triggers — real code blocks from CC
// output are not expected here (headers and questions are short prose).
func escapeMarkdown(s string) string {
	return markdownEscaper.Replace(s)
}

// cardActionPayload is the value object our SendQuestionCard emits and the
// webhook handler expects. Kept package-private; dispatcher never reaches in.
//
// Fields are deliberately minimal — session routing is re-derived from the
// click's chat context, not anything embedded here. A previous SessionKey
// field was removed after security review: it was dead on the inbound path
// but widened the attacker-controlled surface.
type cardActionPayload struct {
	Kind      string `json:"kind"`
	ToolUseID string `json:"tool_use_id"`
	Header    string `json:"header"`
	Label     string `json:"label"`
	// ChatType carries the originating session's chat type ("direct"/"group").
	// Unlike the removed SessionKey field, this is not attacker-derived
	// routing state: it is strictly whitelisted to {"direct","group"} on read
	// (anything else is ignored) and only consulted by the WebSocket card
	// path, whose callback envelope lacks a chat_type. The webhook path reads
	// the real chat_type from the signed envelope and never trusts this.
	// Feishu p2p (1:1) chats also use an "oc_" open_chat_id, so the old
	// prefix heuristic mis-routed 1:1 card answers into a phantom group
	// session — this field lets the answer land back in the direct session.
	ChatType string `json:"chat_type,omitempty"`
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
//  2. v2 card action: {"action":{"value":{...}},"context":{
//     "open_chat_id":"oc_...","open_message_id":"om_..."},"operator":{...}} —
//     the larksuite oapi-sdk-go v3 CardActionTriggerRequest nests
//     open_chat_id/open_message_id inside a `context` object; the top level
//     carries neither. Without the fallback below the chat/message ids decode
//     to "" and the answer routes into a phantom direct session (#2006).
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
	// v2 envelopes nest the ids under `context`; fall back when the top-level
	// (v1) fields are absent so both shapes resolve real routing ids (#2006).
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
	// Resolve chat type. The v2 card.action.trigger envelope carries no
	// chat_type at any level, so the webhook path (like the WS path) must fall
	// back to the chat_type embedded in the button value before defaulting to
	// "direct" — otherwise a group card answer routes into a phantom direct
	// session and the originating group session never sees the reply (#2007).
	ct := chatType
	if ct == "" {
		ct = normalizeCardChatType(val.ChatType)
	}
	if ct != "group" {
		ct = "direct"
	}
	// Stable-ish dedup key. Lark SDK can re-deliver the same card_action on
	// WS reconnect; without this, a click gets forwarded twice. Derive the
	// id from (open_message_id, operator, tool_use_id) — same card + same
	// clicker + same question collapses to one key, so replays dedup.
	// "card_action:" prefix guards against collision with message event IDs
	// in the same Dedup bucket.
	eventID := ""
	if messageID != "" {
		// Cap each user-controlled component before composition so a
		// pathological Feishu payload (e.g. an unusually long open_id)
		// can't blow past the SanitizeForLog truncation cap and shadow
		// the message ID in the dedup key. Real Feishu open_ids are
		// well under 64 bytes; the 64-cap is permissive.
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
		// Card click on a question targeted to this bot should always route
		// through dispatch regardless of group mention_only gating — the
		// user explicitly interacted with the bot's card.
		MentionMe: true,
	}
	// Best-effort: edit the original card so the chosen option is visible
	// and buttons go away. Failures don't stop dispatching the answer —
	// the conversation continuation matters more than UI polish.
	if messageID != "" && f.cfg.AppID != "" {
		// Strip control / bidi before interpolating into the edit markdown —
		// escapeMarkdown only handles emphasis metacharacters, not C1 / bidi
		// overrides / LS / PS that could corrupt Feishu's rendering.
		safeText := osutil.SanitizeForLog(text, 1024)
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
			// Detach from the webhook/WS callback ctx — that ctx can be
			// cancelled the moment the transport layer's outer handler
			// returns, even though the message dispatch continues in its
			// own goroutine. A 10s independent budget matches the Feishu
			// EditMessage call timeout and keeps the "card visual update"
			// best-effort semantics intact.
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

// composeAskAnswerText turns a card-click payload into the plain user text
// that gets injected as a new user message. Mirrors the dashboard's
// composeAskAnswer format ("Header: Label.") so CC sees a stable reply shape
// regardless of which surface the user answered from.
//
// Both header and label are rune-capped and control-character-stripped
// before composition. A hostile Feishu relay or replayed mutation could
// otherwise land 60 KB / bidi-injected strings on CC stdin. The caps match
// send-time policy so round-trip drift stays bounded.
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
