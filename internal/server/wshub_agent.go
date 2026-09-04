// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     agent tailer block (tailers / wiredLinkersMu / wiredLinkers)
//	READS:      shared deps block (router for session resolution)
package server

import (
	"regexp"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/agentlink"
)

// agentTaskDoneSetter is the server-side view of the parent-stream EventLog
// surface maybeWireLinkerTailer needs: one callback fired when a parent-stream
// `task_done` arrives so the matching agent tailer closes promptly. Declared
// here so the call site never names *cli.EventLog (which satisfies it
// implicitly); another backend can pass its own implementation through
// ManagedSession.AgentEventLog (#625).
type agentTaskDoneSetter interface {
	SetOnAgentTaskDone(fn func(taskID, status string))
}

// enrichSnapshot overlays tailer-local aggregator metrics onto each
// SubagentInfo in snap. No-op when h.tailers is nil (unit test harness).
//
// Precedence: the Snapshot carries what the EventLog recorded from
// parent-stream task_progress; the tailer overwrites only with a later value,
// since it tracks per-agent tool_use count and step duration at finer
// granularity. Once task_done has closed the tailer it is gone from the
// registry and the EventLog values stand.
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
// The linker is consumed via agentlink.AgentLinker so server stays decoupled
// from the *cli.SubagentLinker concrete type.
func (h *Hub) maybeWireLinkerTailer(key string, sess *session.ManagedSession) {
	// Nil-check the concrete return first: a typed-nil *cli.SubagentLinker
	// promoted to an interface value is non-nil at the interface layer.
	concrete := sess.SubagentLinker()
	if concrete == nil || h.tailers == nil {
		return
	}
	// Map dedup runs against (dynamic type, pointer value), so other
	// AgentLinker implementations work without churn.
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

	// Parent stream task_done → close tailer (fires agent_done to remaining
	// subscribers + flushes final meta). Same typed-nil guard on the concrete
	// return as above; after it, route through agentTaskDoneSetter so the call
	// site does not name *cli.EventLog (#625).
	if rawLog := sess.AgentEventLog(); rawLog != nil {
		var hook agentTaskDoneSetter = rawLog
		hook.SetOnAgentTaskDone(func(taskID, status string) {
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
	sess := h.router.SessionFor(msg.Key)
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
