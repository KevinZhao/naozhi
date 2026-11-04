package upstream

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// Files serves the primary's file RPCs (files_list / file_read / files_exists
// / fetch_outputs, planner RFC §15.5 ④) from this node's own roots, under its
// own allowed_root, sensitive-name and size rules; refusals travel in the
// answers' node.FileRefusal. Without List the node does not advertise
// node.CapFiles and the RPCs answer unknown method.
type Files struct {
	List   func(node.FilesListParams) node.FileList
	Read   func(node.FileReadParams) node.FileRead
	Exists func(node.FilesExistsParams) node.FilesExists
	// Outputs returns the session's outputs as GET /api/sessions/outputs
	// answers them.
	Outputs func(key string) any
}

// ServeFiles wires the file RPCs; call it before Run.
func (c *Connector) ServeFiles(f Files) { c.files = f }

// registerCaps is the register frame's capability list.
func (c *Connector) registerCaps() []string {
	caps := derivedCaps()
	if c.files.List != nil {
		caps = append(caps, node.CapFiles)
		slices.Sort(caps)
	}
	return caps
}

// handleFileRequest runs a file RPC; ok is false for a method that is not one.
func (c *Connector) handleFileRequest(req node.ReverseMsg) (res json.RawMessage, ok bool, err error) {
	switch req.Method {
	case "files_list", "file_read", "files_exists", "fetch_outputs":
	default:
		return nil, false, nil
	}
	f := c.files
	if f.List == nil || f.Read == nil || f.Exists == nil || f.Outputs == nil {
		return nil, true, fmt.Errorf("unknown method: %q", req.Method)
	}
	switch req.Method {
	case "files_list":
		var p node.FilesListParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, true, fmt.Errorf("files_list params: %w", err)
		}
		res, err = marshalResult(f.List(p))
	case "file_read":
		var p node.FileReadParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, true, fmt.Errorf("file_read params: %w", err)
		}
		res, err = marshalResult(f.Read(p))
	case "files_exists":
		var p node.FilesExistsParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, true, fmt.Errorf("files_exists params: %w", err)
		}
		res, err = marshalResult(f.Exists(p))
	case "fetch_outputs":
		var p struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, true, fmt.Errorf("fetch_outputs params: %w", err)
		}
		if err := sessionkey.ValidateSessionKey(p.Key); err != nil {
			return nil, true, fmt.Errorf("fetch_outputs key: %w", err)
		}
		res, err = marshalResult(f.Outputs(p.Key))
	}
	return res, true, err
}
