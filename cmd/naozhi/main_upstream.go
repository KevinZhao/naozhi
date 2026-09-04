// Upstream connector Discover/Preview callbacks, as named constructors so the
// empty-slice-on-error and project-backfill logic is unit-testable.
package main

import (
	"encoding/json"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// newUpstreamDiscoverFunc builds the connector's session-discovery callback:
// scans claudeDir minus naozhi-managed pids/sessions/cwds, backfills Project
// via projectMgr, and always returns a non-nil JSON array (empty on scan error).
func newUpstreamDiscoverFunc(claudeDir string, router *session.Router, projectMgr *project.Manager) func() (json.RawMessage, error) {
	return func() (json.RawMessage, error) {
		pids, sids, cwds := router.ManagedExcludeSets()
		sessions, err := discovery.Scan(claudeDir, pids, sids, cwds)
		if err != nil {
			return json.Marshal([]any{})
		}
		if sessions == nil {
			sessions = []discovery.DiscoveredSession{}
		}
		if projectMgr != nil && len(sessions) > 0 {
			paths := make([]string, len(sessions))
			for i, d := range sessions {
				paths[i] = d.CWD
			}
			cwdMap := projectMgr.ResolveWorkspaces(paths)
			for i := range sessions {
				sessions[i].Project = cwdMap[sessions[i].CWD]
			}
		}
		return json.Marshal(sessions)
	}
}

// newUpstreamPreviewFunc builds the connector's history-preview callback; it
// always returns a non-nil JSON array so the connector never forwards null.
func newUpstreamPreviewFunc(claudeDir string) func(sessionID string) (json.RawMessage, error) {
	return func(sessionID string) (json.RawMessage, error) {
		entries, err := discovery.LoadHistory(claudeDir, sessionID, "")
		if err != nil {
			return json.Marshal([]clievent.EventEntry{})
		}
		if entries == nil {
			entries = []clievent.EventEntry{}
		}
		return json.Marshal(entries)
	}
}
