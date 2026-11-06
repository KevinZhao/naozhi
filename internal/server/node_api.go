package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/upstream"
)

// NodePlanner serves this node's Planner to a primary over the reverse link
// (docs/rfc/remote-planner.md §4): the Planner routes for its `api` calls
// while the Planner is enabled, and the snapshots' purpose either way.
func (s *Server) NodePlanner() upstream.Planner {
	p := upstream.Planner{Purpose: s.purpose}
	if s.nodeAPI != nil {
		p.API = s.nodeAPI
	}
	return p
}

// remoteAPITimeout bounds a forwarded call: a node that froze leaves a
// reverse RPC hanging well past a minute before the link is declared dead,
// and the page must say so long before that (remote-planner.md §8 E6).
const remoteAPITimeout = 15 * time.Second

// handleRemoteAPI serves /api/remote/{node}/{path...}: the Planner route
// /api/{path} of the node, run by the node's own handler (node.APICaller).
// Only node.APIRoutes pass, checked here and again on the node.
func (s *Server) handleRemoteAPI(w http.ResponseWriter, r *http.Request) {
	nc, ok := s.nodes.LookupNode(w, r.PathValue("node"))
	if !ok {
		return
	}
	req := node.APIRequest{Method: r.Method, Path: "/api/" + r.PathValue("path"), Query: r.URL.RawQuery}
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, node.MaxAPIBody+1))
		if err != nil {
			errResp(w, http.StatusBadRequest, "body_unreadable", "request body unreadable")
			return
		}
		if len(b) > 0 {
			req.Body = b
		}
	}
	if err := node.CheckAPIRequest(req); err != nil {
		errResp(w, http.StatusNotFound, "remote_route", "not a remote Planner route")
		return
	}
	if req.Path == "/api/planner/send" && carriesFiles(req.Body) {
		// The uploads are in this node's store, which the Planner's node
		// cannot read (remote-planner.md §7).
		errResp(w, http.StatusBadRequest, "remote_files", "发给远端节点 Planner 的消息暂不支持附件")
		return
	}
	caller, ok := nc.(node.APICaller)
	if !ok || !nc.Meta().HasCap(node.CapPlanner) {
		errResp(w, http.StatusNotImplemented, "remote_planner_unsupported", "该节点没有运行 Planner，或版本过旧")
		return
	}
	if nc.Status() != "ok" {
		errResp(w, http.StatusServiceUnavailable, "node_offline", "节点离线")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), remoteAPITimeout)
	defer cancel()
	res, err := caller.CallAPI(ctx, req)
	switch {
	case errors.Is(err, node.ErrAPIUnsupported):
		errResp(w, http.StatusNotImplemented, "remote_planner_unsupported", "该节点没有运行 Planner，或版本过旧")
		return
	case errors.Is(err, context.DeadlineExceeded):
		errResp(w, http.StatusGatewayTimeout, "node_timeout", "节点没有及时回应")
		return
	case err != nil:
		slog.Warn("remote planner api failed", "node", nc.NodeID(), "path", req.Path, "err", err)
		errResp(w, http.StatusBadGateway, "node_error", "节点出错")
		return
	}
	if res.Status < 100 || res.Status > 599 {
		errResp(w, http.StatusBadGateway, "node_error", "节点出错")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(res.Status)
	_, _ = w.Write(res.Body)
}

// carriesFiles reports whether a Planner send names uploads.
func carriesFiles(body []byte) bool {
	var v struct {
		FileIDs []string `json:"file_ids"`
	}
	return json.Unmarshal(body, &v) == nil && len(v.FileIDs) > 0
}
