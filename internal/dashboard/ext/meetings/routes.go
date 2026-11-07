package meetings

import "github.com/naozhi/naozhi/internal/dashboard/httputil"

// Routes returns the endpoints this package owns.
//
// Patterns must stay string literals: routes_snapshot_test.go reads them from
// the AST as the anti-drift gate.
func (h *Handler) Routes() []httputil.Route {
	return []httputil.Route{
		{Pattern: "GET /api/meetings", Handler: h.HandleList},
		{Pattern: "GET /api/meetings/{id}", Handler: h.HandleGet},
		{Pattern: "GET /api/meetings/{id}/audio/{track}", Handler: h.HandleAudio},
		{Pattern: "POST /api/meetings/stop", Handler: h.HandleStop},
		{Pattern: "POST /api/meetings/authorize", Handler: h.HandleAuthorize},
		{Pattern: "POST /api/meetings/{id}/keep", Handler: h.HandleKeep},
		{Pattern: "POST /api/meetings/{id}/retry", Handler: h.HandleRetry},
		{Pattern: "POST /api/meetings/{id}/retranscribe", Handler: h.HandleRetranscribe},
		{Pattern: "DELETE /api/meetings/{id}/audio", Handler: h.HandleDeleteAudio},
	}
}
