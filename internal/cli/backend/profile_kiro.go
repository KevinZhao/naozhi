package backend

import (
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
)

// kiroProfile returns the Profile describing Amazon's kiro CLI. It speaks
// JSON-RPC 2.0 (Agent Client Protocol) and so it constructs a fresh
// ACPProtocol per session. ProtocolDeps is ignored — ACP does not honor a
// claude-style settings override.
//
// RequiredNodeCaps lists "acp" so that reverse-node routing (per
// internal/server/select_node_for_backend.go) can reject hosts that do not
// advertise ACP support before attempting a kiro session there.
func kiroProfile() Profile {
	return Profile{
		ID:            "kiro",
		DisplayName:   "kiro",
		DefaultBinary: "kiro-cli",
		DefaultTag:    "kiro",
		ChipColor:     "#ff7a3a", // saturation orange — distinct from claude purple
		NewProtocol: func(_ ProtocolDeps) cli.Protocol {
			// Per Multi-Backend RFC §10: seed BackendID so the
			// per-backend metric labels recorded by ReadEvent (RPC error)
			// and WriteInterrupt (cancel) are populated correctly.
			return &cli.ACPProtocol{BackendID: "kiro"}
		},
		DetectInProc: func(cmdline string) bool {
			return strings.Contains(cmdline, "kiro")
		},
		RequiredNodeCaps: []string{"acp"},
		// kiro persists each session as ~/.kiro/sessions/cli/<sid>.json[l]
		// (a sidecar JSON metadata file + a JSONL transcript). Consumed
		// by internal/history/kirojsonl. Stored with leading "~/" for
		// the same reason as claude (doctor display).
		HistoryDir: "~/.kiro/sessions/cli/",
		// kiro accrues per-turn metering as ACP-protocol "credits" rather
		// than dollar cost. Dashboard cost cells render unitless with the
		// "credits" suffix.
		CostUnit: "credits",
		// Multi-Backend RFC §8.2 — kiro lacks several claude-only UX
		// features:
		//   - askuser: ACP has no AskUserQuestion equivalent (validate V13)
		//   - passthrough: no replay-user-messages → forced collect mode
		//   - embedded_context: kiro acp 申报 embeddedContext:false
		//   - audio_input: kiro acp 申报 audio:false (still works via
		//     transcribe-then-text path, but the dashboard hint differs)
		// Image input + MCP HTTP are supported. MCP SSE not.
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
