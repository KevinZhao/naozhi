// routes.go — the /api/cron surface this package owns (#2554).
//
// These patterns used to be written out in internal/server/routes.go's
// registerCronRoutes, which meant the server package decided the cron URL
// space. Now they live next to the handlers that serve them; the server only
// applies the middleware chain.
package cron

import "github.com/naozhi/naozhi/internal/dashboard/httputil"

// Routes returns the cron endpoints. Every one is authenticated — cron can
// trigger and delete scheduled work, so none of them sets Public.
//
// Patterns must stay string literals: routes_snapshot_test.go reads them from
// the AST as the anti-drift gate.
func (h *Handlers) Routes() []httputil.Route {
	return []httputil.Route{
		{Pattern: "GET /api/cron", Handler: h.HandleList},
		{Pattern: "POST /api/cron", Handler: h.HandleCreate},
		{Pattern: "PATCH /api/cron", Handler: h.HandleUpdate},
		{Pattern: "DELETE /api/cron", Handler: h.HandleDelete},
		{Pattern: "POST /api/cron/pause", Handler: h.HandlePause},
		{Pattern: "POST /api/cron/resume", Handler: h.HandleResume},
		{Pattern: "POST /api/cron/trigger", Handler: h.HandleTrigger},
		{Pattern: "GET /api/cron/preview", Handler: h.HandlePreview},
		// Run history / transcript / events / snapshot share the run_id path
		// param and the same per-IP rate limit (RateLimits.Runs).
		{Pattern: "GET /api/cron/runs", Handler: h.HandleRunsList},
		{Pattern: "GET /api/cron/runs/{run_id}", Handler: h.HandleRunDetail},
		{Pattern: "GET /api/cron/runs/{run_id}/transcript", Handler: h.HandleRunTranscript},
		{Pattern: "GET /api/cron/runs/{run_id}/events", Handler: h.HandleRunEvents},
		{Pattern: "GET /api/cron/runs/{run_id}/snapshot", Handler: h.HandleRunSnapshot},
		// Human confirmation queue (docs/rfc/agentcore-cloud-sandbox.md §7.4);
		// confirm/replay are POSTs and replay stops the live run first.
		{Pattern: "GET /api/cron/attention", Handler: h.HandleAttentionList},
		{Pattern: "POST /api/cron/runs/{run_id}/confirm", Handler: h.HandleRunConfirm},
		{Pattern: "POST /api/cron/runs/{run_id}/replay", Handler: h.HandleRunReplay},
	}
}
