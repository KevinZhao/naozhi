package cli

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// BackendInfo describes a probed CLI backend available on this host.
//
// ReplyTag / ChipColor are the dashboard-facing fields populated from the
// matching backend.Profile (and optional CLIBackendConfig.ChipColor override)
// at /api/cli/backends serialization time — see internal/server/dashboard_cli.go.
// They live here rather than on backend.Profile so the JSON shape that the
// dashboard.js frontend consumes is one struct, not a join. Multi-backend RFC §8.2.
type BackendInfo struct {
	ID          string `json:"id"`           // "claude" | "kiro"
	DisplayName string `json:"display_name"` // "claude-code" | "kiro"
	Protocol    string `json:"protocol"`     // "stream-json" | "acp"
	Path        string `json:"path,omitempty"`
	Version     string `json:"version,omitempty"`
	Available   bool   `json:"available"`
	// ReplyTag is the short tag (e.g. "cc", "kiro") appended to IM replies
	// and rendered in dashboard chips. Empty when no Profile is registered
	// for the ID (legacy/unknown backend).
	ReplyTag string `json:"reply_tag,omitempty"`
	// ChipColor is a CSS color string the dashboard uses for the backend
	// chip background. Empty falls back to the dashboard's default token
	// (--nz-accent). Format is whatever CSS accepts — "#7c5cff", "var(...)".
	ChipColor string `json:"chip_color,omitempty"`
	// Features mirrors backend.Profile.Features verbatim — the dashboard
	// reads it to gray out controls that the active backend doesn't
	// support (askuser / passthrough / embedded_context / image_input /
	// audio_input / mcp_http / mcp_sse). Missing key == false. Multi-Backend
	// RFC §8.2.
	//
	// IMPORTANT: dashboard-only field. DetectBackendsCtx leaves this nil
	// (the cli package cannot import internal/cli/backend without forming
	// an import cycle). It is filled by the dashboard handler at
	// /api/cli/backends serialisation time — see
	// internal/server/dashboard_cli.go where backend.Get(id).Features
	// is copied into each entry. Callers reading BackendInfo from
	// DetectBackendsCtx directly will observe nil and must treat every
	// feature as false (the safest degrade). R225-CR-7.
	Features map[string]bool `json:"features,omitempty"`
}

// knownBackends enumerates every backend naozhi can drive, in preferred
// default order. New backends (e.g. gemini-cli) get appended here once their
// Protocol implementation lands.
var knownBackends = []BackendInfo{
	{ID: "claude", DisplayName: "claude-code", Protocol: "stream-json"},
	{ID: "kiro", DisplayName: "kiro", Protocol: "acp"},
}

// knownBackendBinaries maps backend.ID → default executable name detectCLI
// probes when callers don't pass an explicit CLIPath. Kept beside
// knownBackends rather than as a field on BackendInfo because BackendInfo's
// JSON shape is consumed by the dashboard and adding an unrelated field
// would broaden the wire contract. The cli package can't import
// internal/cli/backend (cycle), so this is the cli-side mirror of
// backend.Profile.DefaultBinary; backend/profile_*.go remains the
// authoritative source for everything else (DisplayName, Features, etc.).
// R225-CR-2.
var knownBackendBinaries = map[string]string{
	"claude": "claude",
	"kiro":   "kiro-cli",
}

// DetectBackendsCtx probes the filesystem and $PATH for each known backend
// and returns a list of probe results. Backends whose binary cannot be
// located are included with Available=false so the dashboard can surface
// them as unavailable options instead of hiding them.
//
// The ctx is forwarded into detectVersionCtx so a caller-side cancellation
// (e.g. naozhi SIGTERM during startup) aborts the in-flight --version
// subprocess instead of blocking for the full 5s timeout per backend.
// R55-QUAL-004.
func DetectBackendsCtx(ctx context.Context) []BackendInfo {
	out := make([]BackendInfo, 0, len(knownBackends))
	for _, b := range knownBackends {
		info := b
		info.Path = detectCLI(b.ID)
		// detectCLI returns the bare binary name (e.g. "kiro-cli") when
		// nothing is found on disk, which would make detectVersion pay
		// the full 5s subprocess timeout on every missing backend.
		// Short-circuit via os.Stat for obviously-absent binaries so an
		// operator with only claude installed doesn't wait for the kiro
		// probe to time out at every naozhi restart.
		//
		// os.Stat does not search $PATH — when detectCLI returns a bare
		// binary name (installed system-wide but not at a well-known
		// absolute path), Stat fails with ENOENT and the backend is
		// falsely marked unavailable. Fall back to exec.LookPath, which
		// walks $PATH, to distinguish "not installed anywhere" from
		// "installed via $PATH only".
		if _, statErr := os.Stat(info.Path); statErr != nil {
			resolved, lookErr := exec.LookPath(info.Path)
			if lookErr != nil {
				info.Available = false
				out = append(out, info)
				continue
			}
			info.Path = resolved
		}
		info.Version = detectVersionCtx(ctx, info.Path)
		info.Available = info.Version != ""
		out = append(out, info)
	}
	return out
}

// parseVersionOutput extracts the semver-like version token from a
// "<binary> --version" stdout payload.
//
// Output formats observed across our backends:
//
//	claude   → "2.1.143 (Claude Code)"   (version is the first token)
//	kiro     → "kiro-cli 2.3.0"           (version is the second token)
//
// Strategy: walk whitespace-split tokens and return the first one whose
// leading byte is a digit (the canonical semver shape). Anything else —
// build banner, "version" prefix, dash-prefixed suffix — is skipped.
// The 32-byte cap prevents a hostile / malformed --version response from
// blowing up downstream slog attrs and JSON payloads.
//
// Lives in detect.go (not wrapper.go) because version parsing is a
// detection concern: the function is only ever called by detectVersionCtx
// to fill BackendInfo.Version. R228-ARCH-16.
func parseVersionOutput(s string) string {
	for _, tok := range strings.Fields(s) {
		if len(tok) > 0 && tok[0] >= '0' && tok[0] <= '9' {
			if len(tok) > 32 {
				tok = tok[:32]
			}
			return tok
		}
	}
	return ""
}

// SortBackendsAvailableFirst places available backends before unavailable
// ones while preserving the knownBackends order within each group. Callers
// use this for UI rendering so unusable entries drop to the tail.
func SortBackendsAvailableFirst(backends []BackendInfo) {
	// R179-GO-P2: slices.SortStableFunc replaces sort.SliceStable — typed
	// comparator avoids interface{} boxing and matches the rest of the
	// codebase's generic-sort idiom.
	slices.SortStableFunc(backends, func(a, b BackendInfo) int {
		if a.Available == b.Available {
			return 0
		}
		if a.Available {
			return -1
		}
		return 1
	})
}
