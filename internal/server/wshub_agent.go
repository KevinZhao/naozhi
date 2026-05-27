package server

import (
	"regexp"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/agentlink"
)

// agentTaskDoneSetter is the server-package consumer view of the
// parent-stream EventLog surface that maybeWireLinkerTailer needs:
// install a single callback fired when a parent-stream `task_done`
// arrives so the matching agent tailer closes promptly. Declared here
// (not imported from cli) so wshub_agent.go does not type-assert on
// `*cli.EventLog` at the call site — *cli.EventLog satisfies the
// interface implicitly via Go structural typing.
//
// R217-ARCH-2 / R222-ARCH-5 / #625: closes the type-assertion debt at
// the wshub_agent.go boundary. A future ACP / Gemini backend that
// wires its own task-done plumbing can pass an alternative
// implementation through ManagedSession.AgentEventLog without forcing
// the server to learn the new concrete type. The wider lifecycle
// AgentIntrospector reform (also tracked under #625) lives in the
// session package; this is the minimal server-side cut.
type agentTaskDoneSetter interface {
	SetOnAgentTaskDone(fn func(taskID, status string))
}

// enrichSnapshot overlays tailer-local aggregator metrics onto each
// SubagentInfo in snap. Callers already have a *Hub — the tailer registry
// lives there. Safe to call when h.tailers is nil (unit test harness);
// the function is a no-op in that case.
//
// Metric precedence: the session Snapshot carries whatever the EventLog
// recorded from parent-stream task_progress (authoritative when present).
// We overwrite only when the tailer has a later value — the silent tailer
// tracks each agent's internal tool_use count and per-step duration which
// the parent stream reports only at coarser granularity. If the parent
// stream task_done has already closed the tailer, it's gone from the
// registry and we leave the EventLog values in place (by design).
func (h *Hub) enrichSnapshot(snap *session.SessionSnapshot) {
	if h == nil || h.tailers == nil || snap == nil || len(snap.Subagents) == 0 {
		return
	}
	for i := range snap.Subagents {
		taskID := snap.Subagents[i].TaskID
		if taskID == "" {
			continue
		}
		h.tailers.mu.RLock()
		t := h.tailers.byTask[tailerKey{snap.Key, taskID}]
		h.tailers.mu.RUnlock()
		if t == nil {
			continue
		}
		meta := t.MetaSnapshot()
		if meta.LastTool != "" {
			snap.Subagents[i].LastTool = meta.LastTool
		}
		if meta.LastDetail != "" {
			snap.Subagents[i].LastDetail = meta.LastDetail
		}
		if meta.ToolUses > snap.Subagents[i].ToolUses {
			snap.Subagents[i].ToolUses = meta.ToolUses
		}
		if meta.DurationMS > snap.Subagents[i].DurationMS {
			snap.Subagents[i].DurationMS = meta.DurationMS
		}
	}
}

// maybeWireLinkerTailer installs the server-side OnResolve handler onto
// sess's linker exactly once per AgentLinker, and registers a task_done
// hook on the event log so tailers close promptly when the parent stream
// signals completion. The handler kicks off a silent agentTailer on
// successful resolution so parallel-stream events start buffering
// immediately, even before any client subscribes.
//
// The linker is consumed via the agentlink.AgentLinker interface (R239-
// ARCH-I) so server stays decoupled from the *cli.SubagentLinker concrete
// type. *cli.SubagentLinker satisfies the interface implicitly; future
// ACP / Gemini backends that lack a subagent-linking concept can plug a
// noop AgentLinker without server-side branching.
func (h *Hub) maybeWireLinkerTailer(key string, sess *session.ManagedSession) {
	// Capture the concrete return for the nil check first — a typed nil
	// *cli.SubagentLinker promoted to an interface value is non-nil at
	// the interface layer, so we must guard against `nil` while still in
	// the concrete return type to match the pre-R239-ARCH-I behaviour.
	concrete := sess.SubagentLinker()
	if concrete == nil || h.tailers == nil {
		return
	}
	// Promote to interface for storage and downstream calls. Map dedup
	// runs against (dynamic type, pointer value); behaviour is equivalent
	// to the prior pointer-keyed map for the cli backend but admits other
	// AgentLinker implementations without churn.
	var linker agentlink.AgentLinker = concrete
	h.wiredLinkersMu.Lock()
	if h.wiredLinkers == nil {
		// Hub shutting down — skip.
		h.wiredLinkersMu.Unlock()
		return
	}
	if _, ok := h.wiredLinkers[linker]; ok {
		h.wiredLinkersMu.Unlock()
		return
	}
	h.wiredLinkers[linker] = struct{}{}
	h.wiredLinkersMu.Unlock()

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
		h.tailers.ensureTailer(key, taskID, toolUseID, info.JSONLPath)
	})

	// Parent stream task_done → close tailer (fires agent_done to any
	// remaining subscribers + flushes final meta).
	//
	// We retain the concrete return for the typed-nil guard (a
	// *cli.EventLog promoted to an interface keeps the dynamic-type
	// half non-nil even when the value half is nil — the same trap
	// SubagentLinker dodges above). After the guard we route through
	// the local agentTaskDoneSetter interface so the call site does
	// not name *cli.EventLog. R217-ARCH-2 / #625.
	if rawLog := sess.AgentEventLog(); rawLog != nil {
		var hook agentTaskDoneSetter = rawLog
		hook.SetOnAgentTaskDone(func(taskID, status string) {
			h.tailers.closeTask(key, taskID, status)
		})
	}
}

// wshub_agent.go — WS handlers for agent_subscribe / agent_unsubscribe.
// Paired with agent_tailer.go (the server-side event fanout) and
// dashboard_agent_events.go (the HTTP fallback) to deliver the agent-team
// internal-view feature described by RFC v4 agent-team-ui §3.5.2.

// agentTaskIDRe mirrors the HTTP endpoint's whitelist (taskIDRe) so a WS
// payload with a rogue task_id gets rejected before reaching the Linker.
// Kept local rather than importing the HTTP regex so tests that exercise
// only the WS layer don't drag server.handler state in.
var agentTaskIDRe = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

func (h *Hub) handleAgentSubscribe(c *wsClient, msg node.ClientMsg) {
	if err := session.ValidateSessionKey(msg.Key); err != nil {
		c.SendJSON(node.ServerMsg{Type: "error", Error: "invalid key"})
		return
	}
	if !agentTaskIDRe.MatchString(msg.TaskID) {
		c.SendJSON(node.ServerMsg{Type: "error", Error: "invalid task_id"})
		return
	}
	// Remote-node agent subscriptions are not yet supported — the tailer
	// needs local filesystem access. Emit rejected so the dashboard falls
	// back to the HTTP endpoint (which rejects remote with 404 today,
	// same effective UX).
	if msg.Node != "" && msg.Node != "local" {
		c.SendJSON(node.ServerMsg{
			Type:   "agent_subscribe_rejected",
			Key:    msg.Key,
			TaskID: msg.TaskID,
			Reason: "remote_not_supported",
		})
		return
	}
	sess := h.router.GetSession(msg.Key)
	if sess == nil {
		c.SendJSON(node.ServerMsg{
			Type:   "agent_subscribe_rejected",
			Key:    msg.Key,
			TaskID: msg.TaskID,
			Reason: "session_not_found",
		})
		return
	}
	linker := sess.SubagentLinker()
	if linker == nil {
		c.SendJSON(node.ServerMsg{
			Type:   "agent_subscribe_rejected",
			Key:    msg.Key,
			TaskID: msg.TaskID,
			Reason: "no_linker",
		})
		return
	}
	info, ok := linker.QueryOrResolveFast(msg.TaskID)
	if !ok {
		// Linker context not yet installed (awaiting init event). The HTTP
		// endpoint returns 202 on the same condition; tell WS clients to
		// retry once the polling loop settles.
		c.SendJSON(node.ServerMsg{
			Type:   "agent_subscribe_rejected",
			Key:    msg.Key,
			TaskID: msg.TaskID,
			Reason: "pending",
		})
		return
	}
	if info.InternalAgentID == "" || info.JSONLPath == "" {
		c.SendJSON(node.ServerMsg{
			Type:   "agent_subscribe_rejected",
			Key:    msg.Key,
			TaskID: msg.TaskID,
			Reason: "tombstone",
		})
		return
	}
	// toolUseID isn't strictly needed by the tailer (all lookups use taskID)
	// but we thread it through for log correlation. attach_subscribe does
	// not expose it on the WS layer.
	t, ok := h.tailers.ensureTailer(msg.Key, msg.TaskID, "", info.JSONLPath)
	if !ok || t == nil {
		c.SendJSON(node.ServerMsg{
			Type:   "agent_subscribe_rejected",
			Key:    msg.Key,
			TaskID: msg.TaskID,
			Reason: "capacity",
		})
		return
	}
	if !h.tailers.attach(tailerKey{msg.Key, msg.TaskID}, c) {
		c.SendJSON(node.ServerMsg{
			Type:   "agent_subscribe_rejected",
			Key:    msg.Key,
			TaskID: msg.TaskID,
			Reason: "closed",
		})
		return
	}
}

func (h *Hub) handleAgentUnsubscribe(c *wsClient, msg node.ClientMsg) {
	if err := session.ValidateSessionKey(msg.Key); err != nil {
		return
	}
	if !agentTaskIDRe.MatchString(msg.TaskID) {
		return
	}
	if h.tailers == nil {
		return
	}
	h.tailers.detach(tailerKey{msg.Key, msg.TaskID}, c)
}
