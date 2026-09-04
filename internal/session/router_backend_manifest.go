// Package session backend-manifest assembly.
//
// BackendsManifest / BackendsList are the single source of the {backends,
// default, detected} payload that /api/cli/backends serves, shared by the
// dashboard HTTP handler (internal/dashboard/ext/cli) and the reverse-RPC
// "fetch_backends" branch (internal/upstream): a reverse node must report the
// same backend list the primary would show, or the node-aware picker picks
// the wrong default.
//
// This lives in session (not cli) because it reads the per-backend Profile via
// internal/cli/backend, which cli cannot import without a cycle (see the
// BackendInfo godoc in internal/cli/detect.go).
package session

import (
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/backend"
)

// BackendManifest is the wire shape of GET /api/cli/backends; the field tags
// are the dashboard.js contract ({backends, default, detected}).
type BackendManifest struct {
	Backends []cli.BackendInfo `json:"backends"`
	Default  string            `json:"default"`
	Detected []cli.BackendInfo `json:"detected"`
}

// BackendsList returns the configured (spawnable) backends for this router,
// each annotated with the CLI metadata its wrapper collected plus the
// dashboard-facing Profile fields (ReplyTag / ChipColor / Features).
//
// The ordering is BackendIDs()'s: default backend first, remainder sorted.
func (r *Router) BackendsList() []cli.BackendInfo {
	ids := r.BackendIDs()
	backends := make([]cli.BackendInfo, 0, len(ids))
	for _, id := range ids {
		info := cli.BackendInfo{ID: id, Available: true}
		// BackendModelManifest takes r.mu internally; BackendsList runs
		// unlocked (handler / reverse-RPC context), so no lock nesting.
		info.Models = r.BackendModelManifest(id)
		if wr := r.BackendWrapper(id); wr != nil {
			info.DisplayName = wr.CLIName
			// Path intentionally omitted — installed-binary paths leak host
			// filesystem layout to any authenticated dashboard user.
			// EffectiveVersion (not spawn-time CLIVersion) so a host CLI
			// upgrade under a long-lived naozhi surfaces here too.
			ver := wr.EffectiveVersion()
			info.Version = ver
			if wr.Protocol != nil {
				info.Protocol = wr.Protocol.Name()
			}
			// Version=="" (binary present but --version parse failed) must
			// not masquerade as Available=true — dashboard greys it out.
			info.Available = ver != ""
		}
		// Unknown ids leave the Profile fields empty — dashboard falls back
		// to default tokens and treats every feature as false.
		if p, ok := backend.Get(id); ok {
			info.ReplyTag = p.DefaultTag
			info.ChipColor = p.ChipColor
			if len(p.Features) > 0 {
				// Defensive copy — Profile.Features is the registry's
				// authoritative map; never hand out the shared reference.
				info.Features = make(map[string]bool, len(p.Features))
				for k, v := range p.Features {
					info.Features[k] = v
				}
			}
		}
		backends = append(backends, info)
	}
	return backends
}

// BackendsManifest assembles the full {backends, default, detected} payload.
// detected is passed in (not probed here) because probing spawns a --version
// subprocess per backend binary; callers pre-compute it once at construction
// to avoid a fork-storm per request. nil serialises as an empty "detected".
func (r *Router) BackendsManifest(detected []cli.BackendInfo) BackendManifest {
	if detected == nil {
		// Keep the JSON array non-null so the frontend's Array.isArray
		// guard on `detected` holds.
		detected = []cli.BackendInfo{}
	}
	return BackendManifest{
		Backends: r.BackendsList(),
		Default:  r.DefaultBackend(),
		Detected: detected,
	}
}
