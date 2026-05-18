package backend

import (
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
)

// kiroProfile returns the Profile describing Amazon's kiro CLI. It speaks
// JSON-RPC 2.0 (Agent Client Protocol) and so it constructs a fresh
// ACPProtocol per session. ProtocolDeps.SettingsFile / RefreshSettings are
// ignored — ACP does not honor a claude-style settings override.
//
// RequiredNodeCaps lists "acp" so that reverse-node routing (Sprint 1b) can
// reject hosts that do not advertise ACP support before attempting a kiro
// session there.
func kiroProfile() Profile {
	return Profile{
		ID:            "kiro",
		DisplayName:   "kiro",
		DefaultBinary: "kiro-cli",
		DefaultTag:    "kiro",
		NewProtocol: func(_ ProtocolDeps) cli.Protocol {
			// Multi-Backend RFC §10 (Sprint 6a): seed BackendID so the
			// per-backend metric labels recorded by ReadEvent (RPC error)
			// and WriteInterrupt (cancel) are populated correctly.
			return &cli.ACPProtocol{BackendID: "kiro"}
		},
		DetectInProc: func(cmdline string) bool {
			return strings.Contains(cmdline, "kiro")
		},
		RequiredNodeCaps: []string{"acp"},
	}
}
