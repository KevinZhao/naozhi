package cliinfo

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
	// fallback. Dashboard-only; cli.DetectBackendsCtx leaves it nil.
	Models []ModelInfo `json:"models,omitempty"`
	// ReplyTag is the short tag (e.g. "cc", "kiro") appended to IM replies and
	// dashboard chips; empty when no Profile is registered for the ID.
	ReplyTag string `json:"reply_tag,omitempty"`
	// ChipColor is the CSS color for the backend chip background; empty falls
	// back to the dashboard's default token (--nz-accent).
	ChipColor string `json:"chip_color,omitempty"`
	// Features mirrors backend.Profile.Features verbatim so the dashboard can gray
	// out controls the backend lacks; missing key == false. Dashboard-only:
	// cli.DetectBackendsCtx leaves it nil (cli cannot import internal/cli/backend —
	// cycle), and readers of that output must treat nil as all-false.
	Features map[string]bool `json:"features,omitempty"`
}

// ModelInfo is the protocol-agnostic model-manifest entry naozhi caches and
// serves via /api/cli/backends. JSON tags are the dashboard wire shape.
type ModelInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}
