package platform

import (
	"fmt"
	"strings"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
)

// AskAnswerKind discriminates an AskUserQuestion answer click from other
// interactive payloads an adapter may embed in the same button field.
const AskAnswerKind = "ask_answer"

// Rune caps for AskAnswerPayload fields: they round-trip through the IM
// button, and without caps a crafted relay could stuff ~60 KB into CC stdin.
const (
	AskLabelMaxRunes  = 512 // labels may include descriptions
	AskHeaderMaxRunes = 128 // short prose
	AskIDMaxRunes     = 128 // tool_use_id, agent_id
)

// AskAnswerPayload is the button value an AskUserQuestion card embeds and the
// click handler decodes. Session routing is re-derived from the click's chat
// context, never from embedded state. Fields are in tag order so Marshal emits
// the same bytes as the equivalent map; the tags are wire format for cards
// already posted and must not change.
type AskAnswerPayload struct {
	// AgentID routes the answer back to the asking agent session; the
	// dispatcher whitelist-validates it before it can influence routing (#2148).
	AgentID string `json:"agent_id,omitempty"`
	// ChatType ("direct"/"group") is whitelisted on read, for callbacks that
	// carry no chat type of their own.
	ChatType string `json:"chat_type,omitempty"`
	Header   string `json:"header"`
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	// ThreadID carries QuestionCard.ThreadID for callbacks that do not say
	// which thread the card is in; omitted outside a thread.
	ThreadID  string `json:"thread_id,omitempty"`
	ToolUseID string `json:"tool_use_id"`
}

// NewAskAnswerPayload builds the capped, whitelisted value for one option
// button of card's item.
func NewAskAnswerPayload(card QuestionCard, item QuestionItem, opt QuestionOption) AskAnswerPayload {
	return AskAnswerPayload{
		AgentID:   textutil.TruncateRunesNoEllipsis(card.AgentID, AskIDMaxRunes),
		ChatType:  NormalizeAskChatType(card.ChatType),
		Header:    textutil.TruncateRunesNoEllipsis(item.Header, AskHeaderMaxRunes),
		Kind:      AskAnswerKind,
		Label:     textutil.TruncateRunesNoEllipsis(opt.Label, AskLabelMaxRunes),
		ThreadID:  askThreadID(card.ThreadID),
		ToolUseID: textutil.TruncateRunesNoEllipsis(card.ToolUseID, AskIDMaxRunes),
	}
}

// askThreadID drops an over-long thread id rather than clipping it: a
// clipped id names a different thread, or none.
func askThreadID(id string) string {
	if len(id) > AskIDMaxRunes {
		return ""
	}
	return id
}

// NormalizeAskChatType whitelists to {"direct","group"}; anything else returns
// "" so an attacker-relayed value never reaches the session key.
func NormalizeAskChatType(s string) string {
	switch s {
	case "direct", "group":
		return s
	default:
		return ""
	}
}

// ComposeAskAnswerText renders a click as the dashboard's "Header: Label."
// reply shape. Header and label are rune-capped and control-stripped so a
// hostile relay cannot land oversized or bidi-injected strings on CC stdin.
// An empty label yields "", which callers treat as no answer.
func ComposeAskAnswerText(p AskAnswerPayload) string {
	h := strings.TrimSpace(textutil.TruncateRunesNoEllipsis(osutil.SanitizeForLog(p.Header, 0), AskHeaderMaxRunes))
	l := strings.TrimSpace(textutil.TruncateRunesNoEllipsis(osutil.SanitizeForLog(p.Label, 0), AskLabelMaxRunes))
	if l == "" {
		return ""
	}
	if h == "" {
		return l + "."
	}
	return h + ": " + l + "."
}

// RenderAskQuestionPlain lists every question with its numbered options as
// plain text, one block per question, each block starting with "\n".
func RenderAskQuestionPlain(items []QuestionItem) string {
	var b strings.Builder
	for qi, q := range items {
		if q.Header != "" {
			fmt.Fprintf(&b, "\n【%s】", q.Header)
		} else {
			fmt.Fprintf(&b, "\n问题 %d：", qi+1)
		}
		b.WriteString(q.Question)
		b.WriteString("\n")
		for oi, o := range q.Options {
			fmt.Fprintf(&b, "  %d. %s", oi+1, o.Label)
			if o.Description != "" {
				fmt.Fprintf(&b, " — %s", o.Description)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}
