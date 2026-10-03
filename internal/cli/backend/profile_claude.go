package backend

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
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
		// claude --resume reads <claudeDir>/projects/<slug(workspace)>/<sid>.jsonl.
		ResumeTarget: func(_, claudeDir, workspace, sessionID string) string {
			if claudeDir == "" || workspace == "" {
				return ""
			}
			return claudefs.SessionJSONL(claudeDir, workspace, sessionID)
		},
		// claude --resume restores the transcript's last cost-state line.
		ResumedCost: claudeResumedCost,
		TerminalLabel: func(entrypoint string) string {
			if entrypoint == "claude-vscode" {
				return "Claude VS Extension"
			}
			return "Claude CLI"
		},
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

// claudeResumedCost reads the cost-state line `claude --resume` restores.
func claudeResumedCost(target, sessionID string) (float64, map[string]clievent.ModelUsage, bool, error) {
	st, found, err := claudefs.LastCostState(target, sessionID)
	if err != nil || !found {
		return 0, nil, found, err
	}
	var models map[string]clievent.ModelUsage
	if len(st.ModelUsage) > 0 && string(st.ModelUsage) != "null" {
		if err := json.Unmarshal(st.ModelUsage, &models); err != nil {
			return 0, nil, false, fmt.Errorf("decode cost-state modelUsage: %w", err)
		}
	}
	return st.TotalCostUSD, models, true, nil
}
