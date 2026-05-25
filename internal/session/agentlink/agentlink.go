// Package agentlink defines the narrow interface server consumes when it
// reaches into a session's agent-linking layer. The Claude CLI backend
// satisfies it via *cli.SubagentLinker today; future backends (ACP, Gemini
// CLI) can plug a noop implementation so the dashboard agent-team UI does
// not need conditional nil branches per backend type.
//
// History / motivation: docs/TODO.md R239-ARCH-I.
package agentlink

import "github.com/naozhi/naozhi/internal/cli"

// AgentLinker is the subset of *cli.SubagentLinker the server package
// actually consumes. Kept intentionally tiny (4 methods) so a backend
// without a real linker concept can return zero-valued cli.LinkInfo
// (Resolved=false / Internal=""/JSONLPath="") and the existing server
// branches treat it as tombstone — no fake-pointer dance.
//
// Why cli.LinkInfo and not a mirrored struct? Server already imports
// internal/cli for cli.NewTranscriptReader + cli.EventEntry, so reusing
// the existing concrete value type avoids a churn-heavy adapter layer
// for zero behavioural gain. The decoupling that matters is the map-key
// identity and the method set, not the value type each method returns.
type AgentLinker interface {
	// OnResolve appends a callback fired after every Resolve (success or
	// tombstone). Multiple subscribers compose; the cli implementation
	// dispatches them serially. The server registers exactly one per
	// linker (deduped via the wiredLinkers map) to start the silent
	// agent_tailer once Resolve completes.
	OnResolve(fn func(taskID, toolUseID, internalAgentID string))

	// Query returns the cached LinkInfo for taskID without scanning disk.
	// ok=false signals "still resolving" so the server can downgrade to
	// pending (HTTP 202 / WS reject reason="pending").
	Query(taskID string) (cli.LinkInfo, bool)

	// QueryOrResolveFast returns cached info, or runs a single fast-path
	// stat without retries. Used by the HTTP/WS endpoints so a tab that
	// reconnects after a parent-stream task_started got persisted but
	// before any in-process Resolve still gets a direct answer in <1ms.
	QueryOrResolveFast(taskID string) (cli.LinkInfo, bool)

	// ProjectSessionDir returns <projectDir>/<parentSessionID>. Empty
	// string when context is not yet installed (init event not seen).
	// Server uses this as the path-traversal anchor for the tool_result
	// endpoint.
	ProjectSessionDir() string
}
