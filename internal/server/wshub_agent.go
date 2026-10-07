package server

import (
	"regexp"
	"slices"

	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/agentlink"
	"github.com/naozhi/naozhi/internal/subagent"
	"github.com/naozhi/naozhi/internal/wsproto"
)

// agentTaskDoneSetter is the server-side view of the parent-stream EventLog
// surface maybeWireLinkerTailer needs: one callback fired when a parent-stream
// `task_done` arrives so the matching agent tailer closes promptly. Declared
// here so the call site never names *ring.EventLog (which satisfies it
// implicitly); another backend can pass its own implementation through
// ManagedSession.AgentEventLog (#625).
type agentTaskDoneSetter interface {
	SetOnAgentTaskDone(fn func(taskID, status string))
}

// enrichSnapshot overlays tailer-local aggregator metrics onto each
// SubagentInfo in snap (tailerRegistry.enrich).
func (h *Hub) enrichSnapshot(snap *session.SessionSnapshot) {
	if h == nil {
		return
	}
	h.tailers.enrich(snap)
}

// admitSend reports whether owner's per-user send budget admits one more
// send; wsClient consults it after its own per-connection limiter.
func (h *Hub) admitSend(owner string) bool {
	return h.admit.allowSend(owner)
}

// maybeWireLinkerTailer installs the server-side OnResolve handler onto
// sess's linker exactly once per AgentLinker, and registers a task_done
// hook on the event log so tailers close promptly when the parent stream
// signals completion. The handler kicks off a silent agentTailer on
// successful resolution so parallel-stream events start buffering
// immediately, even before any client subscribes.
//
// The linker is consumed via agentlink.AgentLinker so server stays decoupled
// from the *subagent.Linker concrete type.
func (h *Hub) maybeWireLinkerTailer(key string, sess *session.ManagedSession) {
	// Nil-check the concrete returns first: a typed-nil pointer promoted to
	// an interface value is non-nil at the interface layer.
	concrete := sess.SubagentLinker()
	if concrete == nil {
		return
	}
	var taskDone agentTaskDoneSetter
	if rawLog := sess.AgentEventLog(); rawLog != nil {
		taskDone = rawLog
	}
	h.wireLinker(key, concrete, taskDone)
}

// wireLinker installs key's tailer callbacks on linker, once per linker
// (identity is the interface key, so other AgentLinker implementations work
// without churn). taskDone, when set, closes a task's tailer on the parent
// stream's task_done — firing agent_done to remaining subscribers and
// flushing final meta.
func (h *Hub) wireLinker(key string, linker agentlink.AgentLinker, taskDone agentTaskDoneSetter) {
	if !h.tailers.wireOnce(linker) {
		return
	}
	linker.OnResolve(func(taskID, toolUseID, internalAgentID string) {
		if internalAgentID == "" {
			// Tombstone — nothing to tail.
			return
		}
		info, ok := linker.Query(taskID)
		if !ok || info.JSONLPath == "" {
			return
		}
		// Silent tailer: no subscribers yet. refCount stays 0 until a
		// WS agent_subscribe arrives; ensureTailer starts the ticker.
		h.tailers.ensureTailer(key, taskID, toolUseID, info.JSONLPath, nil)
	})
	if taskDone != nil {
		taskDone.SetOnAgentTaskDone(func(taskID, status string) {
			h.tailers.closeTask(key, taskID, status)
		})
	}
}

// WS handlers for agent_subscribe / agent_unsubscribe; agent_tailer.go is the
// event fanout and dashboard_agent_events.go the HTTP fallback.

// agentTaskIDRe mirrors the HTTP endpoint's whitelist (taskIDRe) so a WS
// payload with a rogue task_id gets rejected before reaching the Linker.
// Kept local rather than importing the HTTP regex so tests that exercise
// only the WS layer don't drag server.handler state in.
var agentTaskIDRe = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

func (h *Hub) handleAgentSubscribe(c *wsClient, msg node.ClientMsg) {
	if err := session.ValidateSessionKey(msg.Key); err != nil {
		c.SendJSON(wsproto.NewError(wsproto.Error{Error: "invalid key"}))
		return
	}
	if !agentTaskIDRe.MatchString(msg.TaskID) {
		c.SendJSON(wsproto.NewError(wsproto.Error{Error: "invalid task_id"}))
		return
	}
	// Remote-node agent subscriptions are not yet supported — the tailer
	// needs local filesystem access. Emit rejected so the dashboard falls
	// back to the HTTP endpoint (which rejects remote with 404 today,
	// same effective UX).
	if msg.Node != "" && msg.Node != "local" {
		rejectAgentSubscribe(c, msg, "remote_not_supported")
		return
	}
	sess := h.router.SessionFor(msg.Key)
	if sess == nil {
		rejectAgentSubscribe(c, msg, "session_not_found")
		return
	}
	// A workflow agent is the board's, whether or not a process is alive.
	b := sess.WorkflowBoard()
	if tr, st := b.AgentTranscript(msg.TaskID); st != session.TranscriptNone {
		h.subscribeWorkflowAgent(c, msg, b, tr, st)
		return
	}
	linker := sess.SubagentLinker()
	if linker == nil {
		rejectAgentSubscribe(c, msg, "no_linker")
		return
	}
	info, ok := linker.QueryOrResolveFast(msg.TaskID)
	if !ok {
		// Linker context not yet installed (awaiting init event). The HTTP
		// endpoint returns 202 on the same condition; tell WS clients to
		// retry once the polling loop settles.
		rejectAgentSubscribe(c, msg, "pending")
		return
	}
	if info.InternalAgentID == "" || info.JSONLPath == "" {
		rejectAgentSubscribe(c, msg, "tombstone")
		return
	}
	// toolUseID isn't strictly needed by the tailer (all lookups use taskID)
	// but we thread it through for log correlation. attach_subscribe does
	// not expose it on the WS layer.
	h.attachTailer(c, msg, info.JSONLPath, nil)
}

// subscribeWorkflowAgent tails a workflow agent's transcript once it is
// there and its first line names the agent and its run's session; the fd
// that check opened is closed, the tailer opens through tr.Open.
func (h *Hub) subscribeWorkflowAgent(c *wsClient, msg node.ClientMsg, b *session.WorkflowBoard, tr session.AgentTranscript, st session.TranscriptStatus) {
	if st == session.TranscriptPending {
		rejectAgentSubscribe(c, msg, "pending")
		return
	}
	f, pending, err := subagent.OpenAgentTranscript(tr.Open, tr.RunSessionID, msg.TaskID)
	if pending {
		rejectAgentSubscribe(c, msg, "pending")
		return
	}
	if err != nil {
		rejectAgentSubscribe(c, msg, "tombstone")
		return
	}
	_ = f.Close()
	h.attachTailer(c, msg, tr.Path, &workflowTail{open: tr.Open, done: workflowAgentDone(b, msg.TaskID)})
}

// attachTailer subscribes c to the (shared) tailer of msg's task.
func (h *Hub) attachTailer(c *wsClient, msg node.ClientMsg, path string, wf *workflowTail) {
	t, ok := h.tailers.ensureTailer(msg.Key, msg.TaskID, "", path, wf)
	if !ok || t == nil {
		rejectAgentSubscribe(c, msg, "capacity")
		return
	}
	if !h.tailers.attach(tailerKey{msg.Key, msg.TaskID}, c) {
		rejectAgentSubscribe(c, msg, "closed")
	}
}

func rejectAgentSubscribe(c *wsClient, msg node.ClientMsg, reason string) {
	c.SendJSON(wsproto.NewAgentSubscribeRejected(wsproto.AgentSubscribeRejected{Key: msg.Key, TaskID: msg.TaskID, Reason: reason}))
}

// workflowAgentDone reads agentID's row from b's publication: an attempt
// that ended, an earlier attempt, an ended workflow or a row gone are over.
func workflowAgentDone(b *session.WorkflowBoard, agentID string) func() (string, bool) {
	return func() (string, bool) {
		p := b.Published()
		loc, ok := p.Agent(agentID)
		if !ok {
			return "completed", true
		}
		if !loc.Current {
			return "stopped", true // superseded; the board keeps no outcome
		}
		switch st, _ := p.AgentState(loc); st {
		case workflow.AgentDone, workflow.AgentSkipped:
			return "completed", true
		case workflow.AgentFailed:
			return "error", true
		case workflow.AgentStopped:
			return "stopped", true
		}
		i := slices.IndexFunc(p.Workflows, func(w *workflow.Workflow) bool { return w.TaskID == loc.TaskID })
		if i >= 0 && workflow.IsTerminal(p.Workflows[i].Status) {
			return "stopped", true
		}
		return "", false
	}
}

func (h *Hub) handleAgentUnsubscribe(c *wsClient, msg node.ClientMsg) {
	if err := session.ValidateSessionKey(msg.Key); err != nil {
		return
	}
	if !agentTaskIDRe.MatchString(msg.TaskID) {
		return
	}
	h.tailers.detach(tailerKey{msg.Key, msg.TaskID}, c)
}
