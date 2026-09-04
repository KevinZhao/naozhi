package backend

import (
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
)

// kiroProfile returns the Profile describing Amazon's kiro CLI (JSON-RPC 2.0
// Agent Client Protocol). RequiredNodeCaps lists "acp" so reverse-node routing
// rejects hosts that do not advertise ACP support.
func kiroProfile() Profile {
	return Profile{
		ID:            "kiro",
		DisplayName:   "kiro",
		DefaultBinary: "kiro-cli",
		DefaultTag:    "kiro",
		ChipColor:     "#ff7a3a", // saturation orange — distinct from claude purple
		NewProtocol: func(_ ProtocolDeps) cli.Protocol {
			// Seed BackendID so per-backend metric labels are populated.
			return &cli.ACPProtocol{BackendID: "kiro"}
		},
		DetectInProc: func(cmdline string) bool {
			return strings.Contains(cmdline, "kiro")
		},
		RequiredNodeCaps: []string{"acp"},
		// ~/.kiro/sessions/cli/<sid>.json[l]: JSON sidecar + JSONL transcript,
		// consumed by internal/history/kirojsonl ("~/" kept for doctor display).
		HistoryDir: "~/.kiro/sessions/cli/",
		// Per-turn metering accrues as ACP "credits", not dollars.
		CostUnit: "credits",
		// askuser: no ACP equivalent; passthrough: no replay-user-messages →
		// collect mode; embedded_context / audio_input: kiro acp 申报 false
		// (audio still works via transcribe-then-text). MCP SSE unsupported.
		Features: map[string]bool{
			"askuser":          false,
			"passthrough":      false,
			"embedded_context": false,
			"image_input":      true,
			"audio_input":      false,
			"mcp_http":         true,
			"mcp_sse":          false,
		},
	}
}
