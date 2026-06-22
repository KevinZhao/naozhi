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
		// DetectInProc classifies an OS process as codex. The discovery
		// callers (proc_linux.go / proc_darwin.go detectCLIName) pass the
		// process's BASENAME here — they truncate /proc/PID/cmdline (or `ps
		// -o command=`) at the first NUL/space (argv[0]) before filepath.Base,
		// so the `app-server` subcommand token (argv[1]) is never present.
		// An earlier `&& Contains(cmdline,"app-server")` guard could therefore
		// NEVER match a real codex process → it fell through to "cli". Match on
		// the bare binary name, consistent with how kiro matches "kiro" against
		// the basename "kiro-cli". We cannot (and need not) distinguish
		// `codex login` / `codex exec` from `codex app-server` at the basename
		// level — labelling any codex process as codex in the discovery view is
		// correct; naozhi-managed sessions carry an explicit backend ID and
		// never rely on this sniff.
		DetectInProc: func(cmdline string) bool {
			return strings.Contains(cmdline, "codex")
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
		// Feature values (validated 2026-06-21; embedded_context 2026-06-21 phase2):
		//   - askuser: requestUserInput reverse request not yet card-ified (phase2)
		//   - passthrough: turn/steer not yet wired to /urgent (phase2)
		//   - embedded_context: @file mention works, but via a DIFFERENT
		//     mechanism than claude. claude statically inlines the file
		//     content into the prompt; codex does NOT — the `@path` rides
		//     through verbatim in the turn/start text UserInput and codex
		//     reads the file agentically with its shell tool. Verified
		//     2026-06-21: codex parses `@path` from plain prompt text and
		//     issues a commandExecution to read it. The dashboard gate
		//     (dashboard.js featureForCurrent('embedded_context')) only needs
		//     the backend to "read file paths from inside the prompt", which
		//     codex satisfies. Caveat: resolution depends on the runtime
		//     sandbox permitting the read (codex default is workspace-write,
		//     which does); a read-only sandbox would leave the file unread.
		//     This is honestly a weaker guarantee than claude's static inline,
		//     but matches the dashboard contract and needs zero file-reading
		//     code in naozhi (no new path-traversal / size-cap surface).
		//   - image_input: codex responses accepts data: URL images (gpt-5.x path)
		//   - audio_input: no direct audio
		//   - mcp_http: codex supports HTTP MCP servers
		//   - mcp_sse: not supported
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
