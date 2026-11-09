package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// CapPlanner is the node tag for running the global Planner and answering the
// primary's `api` RPC with its Planner routes (docs/rfc/remote-planner.md
// §4): the primary's Planner page then works the node's Planner in place. A
// direct node declares it in CapsHeader.
const CapPlanner = "planner"

// MaxAPIBody caps an `api` request's body, as POST /api/planner/send caps its
// own.
const MaxAPIBody = 2 << 20

// ErrAPIUnsupported is an `api` call to a node that does not run a Planner or
// predates the RPC; the primary answers 501.
var ErrAPIUnsupported = errors.New("node does not serve the Planner")

// APIRequest is one dashboard API call the primary forwards to a node's own
// handler: Path is the /api/... path, Query the raw query without "?".
type APIRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Query  string `json:"query,omitempty"`
	Body   []byte `json:"body,omitempty"`
}

// APIResponse is the node handler's answer.
type APIResponse struct {
	Status int    `json:"status"`
	Body   []byte `json:"body,omitempty"`
}

// apiRoutes are the routes an `api` call may reach: the Planner page's and
// the session view's Planner-side reads and writes, nothing else. Each is
// "METHOD path"; a node serves them from a mux holding only these, and the
// primary checks the same list before forwarding.
var apiRoutes = map[string]bool{
	"GET /api/planner":          true,
	"POST /api/planner/send":    true,
	"POST /api/planner/state":   true,
	"GET /api/planner/briefs":   true,
	"GET /api/planner/notes":    true,
	"GET /api/inbox":            true,
	"POST /api/inbox/close":     true,
	"POST /api/inbox/decide":    true,
	"PUT /api/inbox/pause":      true,
	"DELETE /api/inbox/pause":   true,
	"GET /api/tasks":            true,
	"POST /api/tasks/state":     true,
	"POST /api/tasks/stop":      true,
	"GET /api/custody":          true,
	"PUT /api/custody":          true,
	"PUT /api/custody/pause":    true,
	"DELETE /api/custody/pause": true,
	"POST /api/custody/undo":    true,
	"GET /api/sessions/notes":   true,
	"GET /api/sessions/digest":  true,
	"GET /api/system/health":    true,
}

// APIRoutes lists the routes an `api` call may reach, "METHOD path" each.
func APIRoutes() []string {
	out := make([]string, 0, len(apiRoutes))
	for r := range apiRoutes {
		out = append(out, r)
	}
	return out
}

// CheckAPIRequest reports why req may not be forwarded or served: a method
// and path outside apiRoutes, a path not in canonical form, a body over
// MaxAPIBody.
func CheckAPIRequest(req APIRequest) error {
	if strings.ContainsAny(req.Path, "\\?#%") || strings.Contains(req.Path, "..") || strings.Contains(req.Path, "//") {
		return fmt.Errorf("api path not canonical: %q", req.Path)
	}
	for i := 0; i < len(req.Path); i++ {
		if req.Path[i] < 0x20 || req.Path[i] == 0x7f {
			return fmt.Errorf("api path not canonical: %q", req.Path)
		}
	}
	if !apiRoutes[req.Method+" "+req.Path] {
		return fmt.Errorf("api route not allowed: %s %q", req.Method, req.Path)
	}
	if strings.ContainsAny(req.Query, "#\x00\r\n") {
		return errors.New("api query not allowed")
	}
	if len(req.Body) > MaxAPIBody {
		return fmt.Errorf("api body too large: %d bytes", len(req.Body))
	}
	return nil
}

// APICaller is a node that forwards dashboard API calls to its own handlers.
type APICaller interface {
	CallAPI(ctx context.Context, req APIRequest) (APIResponse, error)
}

// CallAPI runs req on the node's Planner routes over the reverse conn.
func (c *ReverseConn) CallAPI(ctx context.Context, req APIRequest) (APIResponse, error) {
	if !c.meta.HasCap(CapPlanner) {
		return APIResponse{}, ErrAPIUnsupported
	}
	raw, err := c.rpc(ctx, "api", req)
	if err != nil {
		if strings.Contains(err.Error(), "unknown method") {
			return APIResponse{}, ErrAPIUnsupported
		}
		return APIResponse{}, err
	}
	var res APIResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return APIResponse{}, fmt.Errorf("api answer: %w", err)
	}
	return res, nil
}

// maxAPIAnswer caps a direct node's answer read into memory, as the reverse
// RPC caps its own (maxReverseRPCResponseBytes).
const maxAPIAnswer = maxReverseRPCResponseBytes

// CallAPI forwards req to the direct node's own route over HTTP with its
// token.
func (n *HTTPClient) CallAPI(ctx context.Context, req APIRequest) (APIResponse, error) {
	if !n.planner.Load() {
		return APIResponse{}, ErrAPIUnsupported
	}
	path := req.Path
	if req.Query != "" {
		path += "?" + req.Query
	}
	var body io.Reader
	if req.Body != nil {
		body = bytes.NewReader(req.Body)
	}
	resp, err := n.doRequest(ctx, req.Method, path, body)
	if err != nil {
		return APIResponse{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIAnswer+1))
	if err != nil {
		return APIResponse{}, err
	}
	if len(b) > maxAPIAnswer {
		return APIResponse{}, fmt.Errorf("api answer too large (> %d bytes)", maxAPIAnswer)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return APIResponse{}, fmt.Errorf("node %s refused the token", n.ID)
	}
	return APIResponse{Status: resp.StatusCode, Body: b}, nil
}

// IsAPIRoute reports whether a mux pattern ("METHOD path") is one an `api`
// call may reach.
func IsAPIRoute(pattern string) bool { return apiRoutes[pattern] }
