package cli

import (
	"os"
	"sort"
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

// DetectBackends probes the filesystem and $PATH for each known backend and
// returns a list of probe results. Backends whose binary cannot be located
// are included with Available=false so the dashboard can surface them as
// unavailable options instead of hiding them.
func DetectBackends() []BackendInfo {
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
		if _, statErr := os.Stat(info.Path); statErr != nil {
			info.Available = false
			out = append(out, info)
			continue
		}
		info.Version = detectVersion(info.Path)
		info.Available = info.Version != ""
		out = append(out, info)
	}
	return out
}

// SortBackendsAvailableFirst places available backends before unavailable
// ones while preserving the knownBackends order within each group. Callers
// use this for UI rendering so unusable entries drop to the tail.
func SortBackendsAvailableFirst(backends []BackendInfo) {
	sort.SliceStable(backends, func(i, j int) bool {
		if backends[i].Available != backends[j].Available {
			return backends[i].Available
		}
		return false
	})
}
