package usermsg

import (
	"strings"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/textutil"
)

// turnClass is the stable label of each turn-outcome Code, for logs and
// metrics.
var turnClass = map[Code]string{
	CodeTurnFailed:            "turn_failed",
	CodeTurnMaxTurns:          "max_turns",
	CodeTurnBudget:            "max_budget",
	CodeTurnRefused:           "refused",
	CodeTurnTruncated:         "truncated",
	CodeBackendOverloaded:     "backend_overloaded",
	CodeBackendRateLimited:    "backend_rate_limited",
	CodeBackendAuth:           "backend_auth",
	CodeBackendInvalidRequest: "backend_invalid_request",
	CodeBackendRejected:       "backend_rejected",
}

// maxBackendNameRunes caps the backend ID a notice names.
const maxBackendNameRunes = 32

// ForTurnResult returns the notice for a turn whose result is not an answer,
// and its class label; both "" when no notice is due. A backend rejection
// always gets one, since its text is the raw RPC error. Any other failure gets
// one only when it left no text. An abort never does: neither one claude
// reports as an aborted_* terminal_reason nor the error_during_execution of
// one naozhi asked for. The notice never carries the backend's code or
// wording, and has no emoji, like UserMessage.
func ForTurnResult(r *clievent.SendResult) (text, class string) {
	if r == nil {
		return "", ""
	}
	if be := r.BackendError; be != nil {
		c := classifyBackendError(be)
		name := "后端"
		if be.Backend != "" {
			name = textutil.TruncateRunes(be.Backend, maxBackendNameRunes) + " "
		}
		return name + textForCode(c), turnClass[c]
	}
	if r.Text != "" {
		return "", ""
	}
	c, ok := classifyTurnSubType(r)
	if !ok {
		return "", ""
	}
	return textForCode(c), turnClass[c]
}

// classifyTurnSubType maps an empty-text result onto a turn Code; ok=false
// for a healthy empty turn and for an abort.
func classifyTurnSubType(r *clievent.SendResult) (Code, bool) {
	if r.CLIAborted() {
		return CodeUnknown, false
	}
	switch r.SubType {
	case "error_during_execution":
		if r.Aborted {
			return CodeUnknown, false
		}
		return CodeTurnFailed, true
	case "error_max_turns", "max_turn_requests": // the latter an ACP stopReason
		return CodeTurnMaxTurns, true
	case "error_max_budget_usd":
		return CodeTurnBudget, true
	case "refusal": // ACP stopReason
		return CodeTurnRefused, true
	case "max_tokens": // ACP stopReason
		return CodeTurnTruncated, true
	case "tool_use_failure": // ACP stopReason
		return CodeTurnFailed, true
	}
	if r.IsError || strings.HasPrefix(r.SubType, "error") {
		return CodeTurnFailed, true
	}
	return CodeUnknown, false
}

// classifyBackendError sorts a JSON-RPC rejection or failed backend turn by
// the standard request-error codes, then by wording. -32000 is no signal on
// its own: ACP uses it for "auth required", kiro for anything.
func classifyBackendError(be *clievent.BackendError) Code {
	switch be.Code {
	case -32700, -32600, -32601, -32602:
		return CodeBackendInvalidRequest
	}
	msg := strings.ToLower(be.Message)
	switch {
	case containsAny(msg, "rate limit", "rate_limit", "ratelimit", "throttl", "too many requests", "429"):
		return CodeBackendRateLimited
	case containsAny(msg, "unauthor", "authenticat", "auth required", "credential", "expired token",
		"token expired", "token has expired", "not logged in", "login"):
		return CodeBackendAuth
	case containsAny(msg, "overload", "capacity", "unavailable"):
		return CodeBackendOverloaded
	case be.Code == -32001: // codex app-server: server overloaded
		return CodeBackendOverloaded
	default:
		return CodeBackendRejected
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
