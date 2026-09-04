package cli

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// BackendInfo describes a probed CLI backend available on this host. The
// dashboard-facing fields (ReplyTag / ChipColor / Features / Models) are filled
// from backend.Profile by session.Router.BackendsList at /api/cli/backends time;
// they live here so dashboard.js consumes one struct, not a join (RFC §8.2).
type BackendInfo struct {
	ID          string `json:"id"`           // "claude" | "kiro"
	DisplayName string `json:"display_name"` // "claude-code" | "kiro"
	Protocol    string `json:"protocol"`     // "stream-json" | "acp"
	Path        string `json:"path,omitempty"`
	Version     string `json:"version,omitempty"`
	Available   bool   `json:"available"`
	// Models is the model manifest the dashboard's per-session model popover
	// offers: agent-reported (kiro availableModels) or the cli.backends[].models
	// fallback. Dashboard-only; DetectBackendsCtx leaves it nil.
	Models []ModelInfo `json:"models,omitempty"`
	// ReplyTag is the short tag (e.g. "cc", "kiro") appended to IM replies and
	// dashboard chips; empty when no Profile is registered for the ID.
	ReplyTag string `json:"reply_tag,omitempty"`
	// ChipColor is the CSS color for the backend chip background; empty falls
	// back to the dashboard's default token (--nz-accent).
	ChipColor string `json:"chip_color,omitempty"`
	// Features mirrors backend.Profile.Features verbatim so the dashboard can gray
	// out controls the backend lacks; missing key == false. Dashboard-only:
	// DetectBackendsCtx leaves it nil (cli cannot import internal/cli/backend —
	// cycle), and readers of that output must treat nil as all-false.
	Features map[string]bool `json:"features,omitempty"`

	// defaultBinary is the executable detectCLI probes absent an explicit CLIPath
	// — the cli-side mirror of backend.Profile.DefaultBinary (import cycle), kept
	// on the row so adding a backend is a single-row edit (#408). Unexported: not wire.
	defaultBinary string
}

// knownBackends enumerates every backend naozhi can drive, in preferred order.
// cli-side mirror of backend.Profile.{ID,DefaultBinary} (import cycle);
// detect_backend_mirror_test.go pins ID+binary parity in CI (#408).
var knownBackends = []BackendInfo{
	{ID: "claude", DisplayName: "claude-code", Protocol: "stream-json", defaultBinary: "claude"},
	{ID: "kiro", DisplayName: "kiro", Protocol: "acp", defaultBinary: "kiro-cli"},
	{ID: "codex", DisplayName: "codex", Protocol: "codex-app-server", defaultBinary: "codex"},
}

// lookupBackend returns the knownBackends row for id — the single scan point
// shared by knownBackendBinary / isKnownBackendID / backendDisplayName (#408).
func lookupBackend(id string) (BackendInfo, bool) {
	for _, b := range knownBackends {
		if b.ID == id {
			return b, true
		}
	}
	return BackendInfo{}, false
}

// knownBackendBinary returns the default executable detectCLI probes for the
// backend ID and whether the ID is known; callers fall back to "claude".
func knownBackendBinary(id string) (string, bool) {
	b, ok := lookupBackend(id)
	if !ok {
		return "", false
	}
	return b.defaultBinary, true
}

// DetectBackendsCtx probes the filesystem and $PATH for each known backend.
// Missing backends are included with Available=false so the dashboard can show
// them as unavailable. ctx is forwarded to detectVersionCtx so a startup SIGTERM
// aborts the in-flight --version probe instead of waiting the full 5s per backend.
func DetectBackendsCtx(ctx context.Context) []BackendInfo {
	out := make([]BackendInfo, 0, len(knownBackends))
	for _, b := range knownBackends {
		info := b
		info.Path = detectCLI(b.ID)
		// detectCLI may return a bare name; os.Stat short-circuits absent binaries so
		// a missing backend doesn't pay the 5s --version timeout on every restart. Stat
		// does not search $PATH, so try exec.LookPath before declaring it unavailable.
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

// parseVersionOutput extracts the semver-like token from "<binary> --version"
// output: claude prints "2.1.143 (Claude Code)", kiro prints "kiro-cli 2.3.0",
// so it returns the first whitespace-split token whose leading byte is a
// digit. The 32-byte cap keeps a hostile --version from bloating slog/JSON
// payloads.
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
// ones, preserving knownBackends order within each group (UI rendering).
func SortBackendsAvailableFirst(backends []BackendInfo) {
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
