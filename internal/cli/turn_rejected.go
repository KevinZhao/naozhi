package cli

import "github.com/naozhi/naozhi/internal/cli/clievent"

// TurnRejectedError is what a protocol's ReadEvent returns when the backend
// rejects the in-flight turn over RPC (ACP's session/prompt error, codex's
// deferred turn/start error). The readLoop closes the turn with a failed result
// carrying the error, so the waiting Send returns instead of the session
// staying "running". Err keeps the protocol's own sentinel matchable.
//
// A backend that can reject a turn this way needs a rejection fixture in
// internal/cli/backend's every-backend test, which fails for a registered
// backend without one.
type TurnRejectedError struct {
	// Backend is the backend ID the synthesized result is tagged with.
	Backend string
	// Code and Message are the JSON-RPC error's, Message already sanitized.
	Code    int
	Message string
	Err     error
}

func (e *TurnRejectedError) Error() string { return e.Err.Error() }

func (e *TurnRejectedError) Unwrap() error { return e.Err }

// resultPrefix is the tag the synthesized result text starts with, e.g.
// "[kiro] "; empty when the protocol was built without a backend ID.
func (e *TurnRejectedError) resultPrefix() string {
	if e.Backend == "" {
		return ""
	}
	return "[" + e.Backend + "] "
}

// resultEvent is the failed result readLoop closes the rejected turn with.
func (e *TurnRejectedError) resultEvent() clievent.Event {
	return clievent.Event{
		Type:         "result",
		SubType:      "error",
		Result:       e.resultPrefix() + e.Error(),
		IsError:      true,
		BackendError: &clievent.BackendError{Backend: e.Backend, Code: e.Code, Message: e.Message},
	}
}
