// Package session backend-manifest assembly.
//
// BackendsManifest / BackendsList centralise the {backends, default,
// detected} payload that /api/cli/backends serves. Extracted so BOTH the
// dashboard HTTP handler (internal/dashboard/ext/cli) and the reverse-RPC
// "fetch_backends" branch (internal/upstream) render an identical shape
// from a single source of truth — a reverse node must report the same
// backend list the primary would show for a local session, or the
// node-aware picker (which drove this extraction) would still pick the
// wrong default. See the picker node-aware fix.
//
// This assembly lives in session (not cli) because it reads the per-backend
// Profile (ReplyTag / ChipColor / Features) via internal/cli/backend, which
// the cli package cannot import without an import cycle (see the BackendInfo
// godoc in internal/cli/detect.go). session already depends on backend.
package session

import (
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/backend"
)

// BackendManifest is the wire shape of GET /api/cli/backends. Field tags
// mirror the map[string]any the handler used to build inline so the
// dashboard.js contract ({backends, default, detected}) is unchanged.
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
// This is the exact loop the dashboard handler previously inlined; it is
// now the single source both the handler and the reverse-RPC branch call.
func (r *Router) BackendsList() []cli.BackendInfo {
	ids := r.BackendIDs()
	backends := make([]cli.BackendInfo, 0, len(ids))
	for _, id := range ids {
		info := cli.BackendInfo{ID: id, Available: true}
		if wr := r.BackendWrapper(id); wr != nil {
			info.DisplayName = wr.CLIName
			// Path intentionally omitted — revealing installed-binary paths
			// to any authenticated dashboard user leaks host filesystem
			// layout (mirrors the handler's redaction rationale).
			//
			// EffectiveVersion (not the spawn-time CLIVersion) so a host
			// CLI upgrade under a long-lived naozhi surfaces here too; the
			// dashboard's pending-session card falls back to this value.
			ver := wr.EffectiveVersion()
			info.Version = ver
			if wr.Protocol != nil {
				info.Protocol = wr.Protocol.Name()
			}
			// Version=="" (binary present but --version parse failed) must
			// not masquerade as Available=true — dashboard greys it out.
			info.Available = ver != ""
		}
		// Multi-Backend RFC §8.2: chip color + reply tag + features come
		// from the per-backend Profile registry. Unknown ids leave the
		// fields empty — dashboard falls back to default tokens and treats
		// every feature as false (most conservative degrade).
		if p, ok := backend.Get(id); ok {
			info.ReplyTag = p.DefaultTag
			info.ChipColor = p.ChipColor
			if len(p.Features) > 0 {
				// Defensive copy — Profile.Features is the registry's
				// authoritative map; serialising the same reference into
				// every response would let a buggy caller mutate it.
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
// detected is passed in (not probed here) because probing spawns a
// --version subprocess per backend binary and callers pre-compute it once
// at construction to avoid a fork-storm on every request — the handler
// caches it on Handler.detected, the reverse connector on its own field.
// Callers that have no detected list pass nil (serialises to an empty
// "detected" via the struct's slice zero value handling below).
func (r *Router) BackendsManifest(detected []cli.BackendInfo) BackendManifest {
	if detected == nil {
		// Keep the JSON array non-null so the frontend's Array.isArray
		// guard on `detected` holds even for a reverse node that chose
		// not to ship a detected probe.
		detected = []cli.BackendInfo{}
	}
	return BackendManifest{
		Backends: r.BackendsList(),
		Default:  r.DefaultBackend(),
		Detected: detected,
	}
}
