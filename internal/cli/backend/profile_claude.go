package backend

import (
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
)

// claudeProfile returns the Profile describing Anthropic's claude-code CLI
// (stream-json over stdin/stdout, the default backend).
//
// DetectInProc excludes any cmdline mentioning "kiro": some kiro-cli builds
// embed "claude" in their binary path or help text.
func claudeProfile() Profile {
	return Profile{
		ID:            "claude",
		DisplayName:   "claude-code",
		DefaultBinary: "claude",
		DefaultTag:    "cc",
		ChipColor:     "#7c5cff", // accent purple, mirrors --nz-accent default token
		NewProtocol: func(_ ProtocolDeps) cli.Protocol {
			return &cli.ClaudeProtocol{}
		},
		DetectInProc: func(cmdline string) bool {
			return strings.Contains(cmdline, "claude") && !strings.Contains(cmdline, "kiro")
		},
		// Baseline backend; reverse-nodes need no special capability flag.
		RequiredNodeCaps: nil,
		// Session JSONL under ~/.claude/projects/ ("~/" kept for doctor display).
		HistoryDir: "~/.claude/projects/",
		// Process.TotalCost reports cumulative spend in USD.
		CostUnit: "USD",
		// Full naozhi UX surface; audio goes through Transcribe before the CLI.
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
