package backend

import (
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
)

// claudeProfile returns the Profile describing Anthropic's claude-code CLI.
// It speaks the stream-json protocol over stdin/stdout and is the historical
// default backend.
//
// DetectInProc deliberately excludes any cmdline mentioning "kiro": some
// kiro-cli builds embed the string "claude" in their binary path or
// help text, and we want kiro processes to be classified as kiro, not as
// claude. The exclusion is cheap and tightens the predicate without
// changing the legitimate match path.
func claudeProfile() Profile {
	return Profile{
		ID:            "claude",
		DisplayName:   "claude-code",
		DefaultBinary: "claude",
		DefaultTag:    "cc",
		ChipColor:     "#7c5cff", // accent purple, mirrors --nz-accent default token
		NewProtocol: func(d ProtocolDeps) cli.Protocol {
			return &cli.ClaudeProtocol{
				SettingsFile:    d.SettingsFile,
				RefreshSettings: d.RefreshSettings,
			}
		},
		DetectInProc: func(cmdline string) bool {
			return strings.Contains(cmdline, "claude") && !strings.Contains(cmdline, "kiro")
		},
		// Claude is the baseline backend; reverse-nodes do not need a
		// special capability flag to host claude sessions.
		RequiredNodeCaps: nil,
		// claude-code persists session JSONL under ~/.claude/projects/.
		// Display path stored with leading "~/" so doctor renders it
		// verbatim; callers that need an absolute path expand it
		// themselves via os.UserHomeDir.
		HistoryDir: "~/.claude/projects/",
		// Process.TotalCost reports cumulative spend in USD via the CLI's
		// own metering. Dashboard cost cells render with $ prefix.
		CostUnit: "USD",
		// Multi-Backend RFC §8.2 — claude supports the full naozhi UX
		// surface: AskUserQuestion cards, passthrough multi-message
		// queueing, @-mention embedded context, and image / audio input
		// (the latter goes through Bedrock Transcribe before reaching
		// the CLI). MCP servers connect over HTTP and SSE both.
		Features: map[string]bool{
			"askuser":          true,
			"passthrough":      true,
			"embedded_context": true,
			"image_input":      true,
			"audio_input":      true,
			"mcp_http":         true,
			"mcp_sse":          true,
		},
	}
}
