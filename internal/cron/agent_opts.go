// Package cron — agent_opts.go owns the cron-local view of session-spawn
// types (AgentOpts, Session, SessionStatus, InterruptOutcome) so cron does
// not import internal/session. The adapter in
// internal/wireup/cron_router_adapter.go translates between them and its
// init() panics if the SessionStatus/InterruptOutcome ordinals diverge.
package cron

import "context"

// AgentOpts is the cron-local view of session-spawn parameters — only the
// subset cron's scheduler reads; the wireup adapter translates from
// session.AgentOpts.
//
// ExtraArgs aliasing contract: callers populating AgentOpts for the cron
// Scheduler MUST own ExtraArgs exclusively; the adapter clones the slice on
// the way out so a downstream append() can't corrupt cron's copy.
type AgentOpts struct {
	Backend   string
	Model     string
	Workspace string
	ExtraArgs []string
	// Effort mirrors session.AgentOpts.Effort so a cron job inherits its
	// agent's thinking-effort tier. docs/rfc/kiro-effort-control.md
	Effort string
	// SystemPrompt mirrors session.AgentOpts.SystemPrompt so a cron job
	// inherits agents[<id>].system_prompt (#2493). Cron adds no layer of its own.
	SystemPrompt string
	Exempt       bool
}

// SessionStatus mirrors session.SessionStatus value-for-value; the adapter
// casts numerically, so the iota order must match. The ordinals are
// panic-pinned at boot by the wireup adapter's init() — production inflight
// broadcasts key off the value, so a silent reorder is not safe.
type SessionStatus int

const (
	SessionExisting SessionStatus = iota
	SessionResumed
	SessionNew
)

// Session is the minimum surface cron needs from a live router-spawned
// session: send a turn, query the running CLI session id (so the inflight
// broadcast can fill in SessionID before Send returns, #766), and interrupt
// when the deadline fires. Cron does NOT use attachments or per-turn event
// callbacks; add fields here only when it does.
type Session interface {
	Send(ctx context.Context, text string) (SendResult, error)
	SessionID() string
	InterruptViaControl() InterruptOutcome
}

// SendResult is the cron-local subset of cli.SendResult: Text (IM notify +
// run history), SessionID (stub chain refresh) and CostUSD (per-run cost
// for local runs, which have no SandboxMeta receipt to carry it, #2280).
type SendResult struct {
	Text      string
	SessionID string
	// CostUSD is the CLI's cumulative total_cost_usd for the run (mirrors
	// cli.SendResult.CostUSD); local cron runs persist it onto CronRun.
	CostUSD float64
}

// InterruptOutcome mirrors session.InterruptOutcome value-for-value AND
// ordinal-for-ordinal: the adapter does a numeric cast
// cron.InterruptOutcome(c.s.InterruptViaControl()), so the ordinals must
// match session.InterruptOutcome; the wireup adapter init() panics on
// divergence. executeOpt only branches on Sent and Unsupported; the other
// values exist so the cast stays lossless.
type InterruptOutcome int

const (
	InterruptSent InterruptOutcome = iota
	InterruptNoSession
	InterruptNoTurn
	InterruptUnsupported
	InterruptError
)
