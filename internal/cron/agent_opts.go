// Package cron — agent_opts.go owns the cron-local view of session-spawn
// types so cron does not need to import internal/session.
//
// All types here mirror counterparts in internal/session (AgentOpts,
// ManagedSession, SessionStatus, InterruptOutcome). The production
// adapter in cmd/naozhi/cron_router_adapter.go translates between them;
// an init() panic in that adapter pins the InterruptOutcome ordinals
// against session.InterruptOutcome so a divergence crashes the binary
// at boot rather than silently miscasting.
//
// Why local types: keeping these in cron breaks the cron → session
// reverse import edge that historically forced a full session graph
// rebuild for every cron change. New session-side fields don't ripple
// here unless cron actually consumes them.
package cron

import "context"

// AgentOpts is the cron-local view of session-spawn parameters. Field
// set is INTENTIONALLY a subset of session.AgentOpts: only what cron's
// scheduler actually reads. Adapter translates session.AgentOpts →
// cron.AgentOpts at the cmd/naozhi boundary.
//
// ExtraArgs aliasing contract: callers populating AgentOpts to feed the
// cron Scheduler MUST own ExtraArgs exclusively. The adapter clones
// the slice on the way out to session.AgentOpts so a downstream
// append() can't corrupt cron's own copy and vice-versa.
type AgentOpts struct {
	Backend          string
	Model            string
	SystemPromptFile string
	Workspace        string
	ExtraArgs        []string
	Exempt           bool
}

// SessionStatus mirrors session.SessionStatus value-for-value. The
// adapter does cron.SessionStatus(int(session.SessionStatus)); we rely
// on the iota order matching. session.SessionStatus has three values
// (Existing / Resumed / New) — the adapter does not panic-pin these
// because cron does not branch on the value (it only forwards to
// callers that may compare). If session ever reorders, the only
// observable break is misreporting in tests.
type SessionStatus int

const (
	SessionExisting SessionStatus = iota
	SessionResumed
	SessionNew
)

// Session is the minimum surface cron needs from a live router-spawned
// session: send a turn, query the running CLI session id (so the
// inflight broadcast can fill in SessionID before Send returns —
// fix(cron) #766), and (when deadline fires) interrupt. Cron does
// NOT use attachments or per-turn event callbacks today; if that ever
// changes, add fields here then.
//
// The narrow contract makes the adapter trivial — see
// cmd/naozhi/cron_router_adapter.go cronSessionAdapter.
type Session interface {
	Send(ctx context.Context, text string) (SendResult, error)
	SessionID() string
	InterruptViaControl() InterruptOutcome
}

// SendResult is the cron-local subset of cli.SendResult. cron only
// reads Text (for IM notify + run history) and SessionID (for stub
// chain refresh). No alloc-sensitive paths cross this boundary so a
// fresh struct per Send is fine.
type SendResult struct {
	Text      string
	SessionID string
}

// InterruptOutcome mirrors session.InterruptOutcome value-for-value
// AND ordinal-for-ordinal. The adapter does
//
//	cron.InterruptOutcome(c.s.InterruptViaControl())
//
// which is a numeric cast — diverging ordinals would silently shuffle
// values. The init() panic in cmd/naozhi/cron_router_adapter.go pins
// the contract; CI green build proves the ordinals still match.
//
// Five values mirror session.go exactly: Sent / NoSession / NoTurn /
// Unsupported / Error. Cron's executeOpt only branches on Sent and
// Unsupported today (warn-level escalation when watchdog fired but
// interrupt did not land); the other values exist purely so the cast
// remains lossless.
type InterruptOutcome int

const (
	InterruptSent InterruptOutcome = iota
	InterruptNoSession
	InterruptNoTurn
	InterruptUnsupported
	InterruptError
)
