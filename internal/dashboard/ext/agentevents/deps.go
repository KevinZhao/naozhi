// Package agentevents hosts the dashboard /api/sessions/agent_events and
// /api/sessions/tool_result endpoints.
package agentevents

import (
	"github.com/naozhi/naozhi/internal/dashboard/contracts"
	"github.com/naozhi/naozhi/internal/session"
)

// NodeAccessor aliases the shared dashboard contract (#2285).
type NodeAccessor = contracts.NodeAccessor

// WorkflowAgentTranscript is drill-in's workflow board lookup: the
// transcript of agentID in the workflows of key's session, which needs no
// live process. A missing session or board finds nothing.
func WorkflowAgentTranscript(r SessionLookup, key, agentID string) (session.AgentTranscript, session.TranscriptStatus) {
	sess := r.SessionFor(key)
	if sess == nil {
		return session.AgentTranscript{}, session.TranscriptNone
	}
	return sess.WorkflowBoard().AgentTranscript(agentID)
}
