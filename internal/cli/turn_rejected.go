package cli

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
