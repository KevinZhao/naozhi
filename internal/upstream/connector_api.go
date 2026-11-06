package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// Planner is what this node serves the primary of its Planner
// (docs/rfc/remote-planner.md §4): API answers the `api` RPC from a mux
// holding only node.APIRoutes, without the dashboard's auth chain (the link
// is the primary's credential); Purpose stamps fetch_sessions' snapshots as
// /api/sessions stamps them, so a dispatched session groups as one on the
// primary too. A nil API means no Planner runs here: node.CapPlanner is not
// advertised and `api` answers unknown method.
type Planner struct {
	API     http.Handler
	Purpose func(key string) (purpose, title string)
}

// ServePlanner wires the Planner RPC and the snapshots' purpose; call it
// before Run.
func (c *Connector) ServePlanner(p Planner) { c.planner = p }

// listSessions is fetch_sessions' answer: the router's snapshots with their
// purpose.
func (c *Connector) listSessions() []sessionview.SessionSnapshot {
	snaps := c.router.ListSessions()
	if c.planner.Purpose != nil {
		for i := range snaps {
			snaps[i].Purpose, snaps[i].PurposeTitle = c.planner.Purpose(snaps[i].Key)
		}
	}
	return snaps
}

// handleAPI runs one `api` call on the Planner routes. The node checks the
// route itself, whatever the primary checked.
func (c *Connector) handleAPI(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	if c.planner.API == nil {
		return nil, fmt.Errorf("unknown method: %q", "api")
	}
	var req node.APIRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, fmt.Errorf("api params: %w", err)
	}
	if err := node.CheckAPIRequest(req); err != nil {
		return marshalResult(node.APIResponse{Status: http.StatusNotFound, Body: []byte(`{"error":"not found"}`)})
	}
	u := &url.URL{Path: req.Path, RawQuery: req.Query}
	hr, err := http.NewRequestWithContext(ctx, req.Method, u.String(), bytes.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("api request: %w", err)
	}
	if len(req.Body) > 0 {
		hr.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	c.planner.API.ServeHTTP(rec, hr)
	return marshalResult(node.APIResponse{Status: rec.Code, Body: rec.Body.Bytes()})
}
