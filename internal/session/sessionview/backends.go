package sessionview

import "github.com/naozhi/naozhi/internal/cliinfo"

// BackendManifest is the wire shape of GET /api/cli/backends; the field tags
// are the dashboard.js contract ({backends, default, detected}).
type BackendManifest struct {
	Backends []cliinfo.BackendInfo `json:"backends"`
	Default  string                `json:"default"`
	Detected []cliinfo.BackendInfo `json:"detected"`
}
