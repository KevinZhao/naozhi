package cli

import (
	"context"
	"os"
	"os/exec"
	"slices"
)

// BackendInfo describes a probed CLI backend available on this host.
type BackendInfo struct {
	ID          string `json:"id"`           // "claude" | "kiro"
	DisplayName string `json:"display_name"` // "claude-code" | "kiro"
	Protocol    string `json:"protocol"`     // "stream-json" | "acp"
	Path        string `json:"path,omitempty"`
	Version     string `json:"version,omitempty"`
	Available   bool   `json:"available"`
}

// knownBackends enumerates every backend naozhi can drive, in preferred
// default order. New backends (e.g. gemini-cli) get appended here once their
// Protocol implementation lands.
var knownBackends = []BackendInfo{
	{ID: "claude", DisplayName: "claude-code", Protocol: "stream-json"},
	{ID: "kiro", DisplayName: "kiro", Protocol: "acp"},
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
