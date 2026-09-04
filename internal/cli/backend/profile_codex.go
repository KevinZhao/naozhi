package backend

import (
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
)

// codexProfile returns the Profile describing OpenAI's codex CLI (app-server
// JSON-RPC 2.0, NDJSON over stdio). RequiredNodeCaps lists "codex-app-server"
// so reverse-node routing rejects hosts without codex support (mirrors "acp").
func codexProfile() Profile {
	return Profile{
		ID:            "codex",
		DisplayName:   "codex",
		DefaultBinary: "codex",   // npm @openai/codex installs as `codex`
		DefaultTag:    "cdx",     // reply prefix; aligns with cc / kiro / gem
		ChipColor:     "#10a37f", // OpenAI brand green — distinct from claude purple, kiro orange
		NewProtocol: func(_ ProtocolDeps) cli.Protocol {
			// Seed BackendID so per-backend metric labels from ReadEvent are populated.
			return &cli.CodexProtocol{BackendID: "codex"}
		},
		// Discovery passes argv[0]'s BASENAME (cmdline truncated at the first
		// NUL/space), so the `app-server` subcommand token is never present;
		// match the bare binary name like kiro does. Labelling any codex
		// process as codex is correct — managed sessions carry an explicit
		// backend ID and never rely on this sniff.
		DetectInProc: func(cmdline string) bool {
			return strings.Contains(cmdline, "codex")
		},
		RequiredNodeCaps: []string{"codex-app-server"},
		// Threads under ~/.codex/sessions/ ("~/" kept for doctor display).
		HistoryDir: "~/.codex/sessions/",
		// app-server reports per-turn token usage (thread/tokenUsage/updated);
		// no USD figure on the wire.
		CostUnit: "tokens",
		// askuser/passthrough: requestUserInput and turn/steer not yet wired.
		// embedded_context: `@path` rides through verbatim and codex reads the
		// file agentically with its shell tool (needs a sandbox permitting the
		// read; default workspace-write does) — weaker than claude's static
		// inline but satisfies the dashboard gate with no file-reading code here.
		Features: map[string]bool{
			"askuser":          false,
			"passthrough":      false,
			"embedded_context": true,
			"image_input":      true,
			"audio_input":      false,
			"mcp_http":         true,
			"mcp_sse":          false,
		},
	}
}
