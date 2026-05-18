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
	}
}
