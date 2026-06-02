// File: main_upstream.go
//
// R237-ARCH-8 (#590): the upstream connector's Discover/Preview callbacks
// were inline closures in main(). They are pure given (claudeDir, router,
// projectMgr) and carry non-trivial fallback logic (empty-slice-on-error so
// the connector never forwards a nil JSON payload, plus project-workspace
// backfill). Lifting them to named constructors makes that logic unit-
// testable without booting the upstream connector and trims main()'s body.
package main

import (
	"encoding/json"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// newUpstreamDiscoverFunc builds the connector's session-discovery callback.
// It scans claudeDir excluding naozhi-managed pids/sessions/cwds, backfills
// each discovered session's Project via projectMgr (when configured), and
// always returns a non-nil JSON array — on scan error it marshals an empty
// array rather than surfacing the error to the connector, matching the
// original main() behavior.
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

// newUpstreamPreviewFunc builds the connector's history-preview callback.
// Loads the session's transcript from claudeDir and always returns a
// non-nil JSON array — empty on load error or nil result — so the connector
// never forwards a null payload. R237-ARCH-8 (#590).
func newUpstreamPreviewFunc(claudeDir string) func(sessionID string) (json.RawMessage, error) {
	return func(sessionID string) (json.RawMessage, error) {
		entries, err := discovery.LoadHistory(claudeDir, sessionID, "")
		if err != nil {
			return json.Marshal([]cli.EventEntry{})
		}
		if entries == nil {
			entries = []cli.EventEntry{}
		}
		return json.Marshal(entries)
	}
}
