package clievent

// Peer is one cross-session message (CC's ListAgents / SendMessage channel,
// docs/rfc/planner.md §6.2): an inbound one (a user frame whose origin kind is
// "peer") or one the CLI held back (system/peer_message_hold). Name is the
// sender's --name as it appears in ListAgents; the sender chose it, so it is a
// display hint, never an identity or an authorization.
type Peer struct {
	Name string
	// Mode is the sender's permission mode; inbound only.
	Mode string
	// Body is the message text; inbound only.
	Body string
	// State is where a held message stands: "held", then at most one of
	// "released" (queued after all) or "dropped" (never delivered); hold only.
	State string
	// Cause is why the CLI held the message ("explicit-setting" under
	// crossSessionInbound: hold, "mode-mismatch", ...); state "held" only.
	Cause string
	// Outcome is why a dropped message was ("expired", "refused", "dropped",
	// "discarded"); state "dropped" only.
	Outcome string
}

// ModelSwitch is a system frame saying the CLI moved off the requested model:
// model_fallback (Why is its trigger), model_consent_fallback (its choice)
// or model_refusal_fallback ("refusal").
type ModelSwitch struct {
	From, To, Why string
	// Text is the CLI's own explanation, often carrying the API error.
	Text string
}

// Refusal is a system/permission_denied or system/model_refusal_no_fallback
// frame (RFC planner §3.4).
type Refusal struct {
	// Tool is the denied tool; permission_denied only.
	Tool string
	// Reason is permission_denied's decision_reason or a model refusal's
	// api_refusal_category.
	Reason string
	// Text is the rejection message the model got, or the refusal content.
	Text string
}
