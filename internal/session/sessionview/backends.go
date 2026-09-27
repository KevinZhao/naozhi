package sessionview

import "github.com/naozhi/naozhi/internal/cli"

// BackendManifest is the wire shape of GET /api/cli/backends; the field tags
// are the dashboard.js contract ({backends, default, detected}).
type BackendManifest struct {
	Backends []cli.BackendInfo `json:"backends"`
	Default  string            `json:"default"`
	Detected []cli.BackendInfo `json:"detected"`
}
