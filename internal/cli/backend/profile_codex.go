package backend

import (
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
)

// codexProfile returns the Profile describing OpenAI's codex CLI. It speaks
// the codex app-server JSON-RPC 2.0 protocol (NDJSON over stdio) and so it
// constructs a fresh CodexProtocol per session. ProtocolDeps is ignored —
// codex does not honor a claude-style settings override.
//
// RequiredNodeCaps lists "codex-app-server" so reverse-node routing rejects
// hosts that do not advertise codex support before attempting a codex session
// there (mirrors "acp" for kiro). RFC docs/rfc/codex-backend.md §5.
func codexProfile() Profile {
	return Profile{
		ID:            "codex",
		DisplayName:   "codex",
		DefaultBinary: "codex",   // npm @openai/codex installs as `codex`
		DefaultTag:    "cdx",     // reply prefix; aligns with cc / kiro / gem
		ChipColor:     "#10a37f", // OpenAI brand green — distinct from claude purple, kiro orange
		NewProtocol: func(_ ProtocolDeps) cli.Protocol {
			// Seed BackendID so per-backend metric labels emitted by
			// ReadEvent (RPC error) are populated (multi-backend RFC §10).
			return &cli.CodexProtocol{BackendID: "codex"}
		},
		// DetectInProc matches a codex process but excludes the non-app-server
		// subcommands so a transient `codex login` / `codex exec` invocation is
		// not mislabelled as a hosted naozhi session. We only host codex via
		// `codex app-server`, so require that substring.
		DetectInProc: func(cmdline string) bool {
			return strings.Contains(cmdline, "codex") && strings.Contains(cmdline, "app-server")
		},
		RequiredNodeCaps: []string{"codex-app-server"},
		// codex persists threads under ~/.codex/sessions/. Consumed by a
		// future internal/history/codexjsonl source (RFC §4.1, phase1+).
		// Stored with leading "~/" for doctor display, same as claude/kiro.
		HistoryDir: "~/.codex/sessions/",
		// codex app-server reports per-turn token usage via
		// thread/tokenUsage/updated; there is no USD figure on the wire.
		// Dashboard cost cells render unitless with a "tokens" suffix.
		CostUnit: "tokens",
		// RFC §5 phase1 conservative values (validated 2026-06-21):
		//   - askuser: requestUserInput reverse request not yet card-ified (phase2)
		//   - passthrough: turn/steer not yet wired to /urgent (phase2)
		//   - embedded_context: @file mention not yet plumbed (phase1)
		//   - image_input: codex responses accepts data: URL images (gpt-5.x path)
		//   - audio_input: no direct audio
		//   - mcp_http: codex supports HTTP MCP servers
		//   - mcp_sse: not supported
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
