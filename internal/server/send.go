// send.go is the dashboard's send path: sessionSend validates an HTTP or
// WebSocket send and submits it to turn.Orchestrator, the one that runs IM
// turns too. Every such turn goes through turnSender, so the dashboard sees
// running/ready transitions; cron is the only entry that sends without it
// (own notification path via BroadcastCronResult).
package server

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/turn"
)

// sendParams holds parsed input for a session send request (HTTP and WebSocket).
type sendParams struct {
	Key       string
	Text      string
	Images    []clievent.Attachment
	Workspace string
	ResumeID  string
	Backend   string // optional backend ID picked by the dashboard ("" = router default)
	// AccessProfile is the optional access-profile ID picked by the dashboard
	// new-session picker ("" = global default). One-shot: recorded per key and
	// consumed by spawnSession. RFC project-access-profile §8.2.
	AccessProfile string
}

// sendAckStatus describes the immediate ack status for a queued send.
//   - "accepted": caller became the owner; message is processing now.
//   - "queued":   session was busy; message is queued behind the active turn.
type sendAckStatus string

const (
	sendAckAccepted sendAckStatus = "accepted"
	sendAckQueued   sendAckStatus = "queued"
	// sendAckBusy: session busy and queue disabled (MaxDepth<=0), or the
	// server shutting down, so the message was not buffered — the client
	// should retry.
	sendAckBusy sendAckStatus = "busy"
	// sendAckReset: the text was a bare /new or /clear; the key has no
	// session until the next send.
	sendAckReset sendAckStatus = "reset"
)

// errUrgentUsage rejects a bare /urgent (#3004 分叉 6); asyncErrorMessage
// passes its text through to the client.
var errUrgentUsage = sendUsageError("用法：/urgent <紧急消息>")

// sessionSend validates a send request and submits it with origin, which
// hears about the turn. Returns (true, "", nil) for a /clear or /new reset;
// (false, "", err) on validation failure; else the admission status:
// "accepted" (the turn runs now), "queued" (it joins the next merged turn)
// or "busy" (queue disabled or shutting down; not buffered).
func (e *sendEngine) sessionSend(p sendParams, origin turn.Origin) (bool, sendAckStatus, error) {
	cmd, reset, err := e.prepareSend(p)
	if reset || err != nil {
		return reset, "", err
	}
	switch e.submit(p, cmd, origin) {
	case turn.AckOwner, turn.AckDetached:
		return false, sendAckAccepted, nil
	case turn.AckQueued:
		return false, sendAckQueued, nil
	default: // AckDropped (queue disabled, session busy), AckShuttingDown
		return false, sendAckBusy, nil
	}
}

// prepareSend is the part of a send every entry shares before Submit: it
// validates p, runs a bare /clear or /new (reset=true) and records p's
// workspace, backend, access-profile and resume overrides. A reset or an
// error means there is nothing to submit.
func (e *sendEngine) prepareSend(p sendParams) (cmd turn.Cmd, reset bool, err error) {
	key := p.Key
	// ValidateSessionKey rejects C0/C1 controls, bidi overrides, non-UTF-8 and
	// over-long keys: no log-injection primitive via slog / sessions.json.
	if err := session.ValidateSessionKey(key); err != nil {
		return cmd, false, fmt.Errorf("invalid key")
	}

	// /clear and /new without an argument reset (the CLI built-in does not
	// work in stream-json); "/new <arg>" is sent as text. The workspace
	// override goes too: the dashboard re-sends it with the next send.
	cmd = turn.Parse(p.Text)
	if cmd.Kind == turn.CmdReset && cmd.Arg == "" {
		e.turns.Reset(e.ctx, key, true)
		e.notify.BroadcastSessionsUpdate()
		return cmd, true, nil
	}
	if cmd.Kind == turn.CmdUrgentUsage {
		return cmd, false, errUrgentUsage
	}

	var validatedWorkspace string
	if p.Workspace != "" {
		wsPath, err := validateWorkspace(p.Workspace, e.allowedRoot)
		if err != nil {
			// Generic client message: the error chain may embed the resolved
			// path. Warn — rejects are traversal / symlink-escape events.
			// p.Workspace is attacker-influenced, so SanitizeForLog (200-byte
			// cap, same as other attacker-influenced fields).
			slog.Warn("workspace validation failed", "err", err, "workspace", osutil.SanitizeForLog(p.Workspace, 200))
			return cmd, false, fmt.Errorf("invalid workspace")
		}
		validatedWorkspace = wsPath
		// Refuse an empty chat-key prefix (":agentID"): it would persist "" as
		// the override for every GetWorkspace("") lookup.
		if idx := strings.LastIndexByte(key, ':'); idx > 0 {
			e.router.SetWorkspace(key[:idx], wsPath)
		}
	}

	// Dashboard-picked backend override, recorded per key and consumed by
	// spawnSession at the turn's GetOrCreate. Unknown IDs clamp to the router default in
	// wrapperFor; only hostile input is rejected here (keeps it out of logs).
	if p.Backend != "" {
		// Shared isValidBackendID / maxBackendIDLen so HTTP, WS dispatch and
		// node selection accept the same IDs; error text aligned with
		// dashboard_cron.validateCronBackend for dashboard JS substring matching.
		if len(p.Backend) > maxBackendIDLen {
			return cmd, false, fmt.Errorf("backend exceeds %d-byte limit", maxBackendIDLen)
		}
		if !isValidBackendID(p.Backend) {
			return cmd, false, fmt.Errorf("invalid backend identifier")
		}
		e.router.SetSessionBackend(key, p.Backend)
	}

	// Dashboard-picked access-profile override (RFC project-access-profile
	// §8.2); same one-shot semantics as Backend. Unknown IDs resolve to the
	// global default in resolveSpawnParamsLocked; only hostile input is rejected.
	if p.AccessProfile != "" {
		if len(p.AccessProfile) > maxBackendIDLen || !isValidBackendID(p.AccessProfile) {
			return cmd, false, fmt.Errorf("invalid access_profile identifier")
		}
		e.router.SetSessionAccessProfile(key, p.AccessProfile)
	}

	// Bound resume_id length before the regex scan (UUIDs are 36 chars; 64
	// leaves headroom) so a hostile multi-MB value costs nothing.
	if len(p.ResumeID) > 64 {
		return cmd, false, fmt.Errorf("invalid resume_id length")
	}
	if p.ResumeID != "" && claudefs.IsValidSessionID(p.ResumeID) {
		ws := validatedWorkspace
		if ws == "" {
			ws = e.router.DefaultWorkspace()
		}
		e.router.RegisterForResume(key, p.ResumeID, ws, "")
	}

	return cmd, false, nil
}

// submit hands a prepared send to the Orchestrator on the engine's ctx and
// TrackSend; /urgent's text loses its prefix and preempts.
func (e *sendEngine) submit(p sendParams, cmd turn.Cmd, origin turn.Origin) turn.Ack {
	r := turn.Request{Key: p.Key, Text: p.Text, Images: p.Images, Origin: origin}
	if cmd.Kind == turn.CmdUrgent {
		r.Text, r.Priority = cmd.Arg, turn.PriorityNow
	}
	return e.turns.Submit(e.ctx, r, dashAdmission{track: e.TrackSend, ctx: e.ctx})
}

// sessionOptsFor returns the AgentOpts to use when spawning (or resuming)
// the session for key: scratch keys via the pool (inherited config + the
// --append-system-prompt quote shim), everything else via buildSessionOpts.
// The pool lookup touches the scratch's lastUsed — that "Touch on lookup" is
// the only thing preventing the sweeper from evicting a scratch about to
// receive its first send. Do not remove it.
func (e *sendEngine) sessionOptsFor(key string) session.AgentOpts {
	if e.scratchPool != nil && sessionkey.IsScratchKey(key) {
		if opts, ok := e.scratchPool.OptsForKey(key); ok {
			return opts
		}
	}
	return buildSessionOpts(key, e.resolver, e.agents, e.projectMgr)
}
