// Package agentlink defines the narrow interfaces server consumes when it
// reaches into a session's agent-linking layer. The Claude CLI backend
// satisfies them via *cli.SubagentLinker; other backends can plug a noop so
// the dashboard agent-team UI needs no per-backend nil branches. Three
// single-responsibility facets compose into AgentLinker (#375).
package agentlink

import (
	"github.com/naozhi/naozhi/internal/cli"
)

// Notifier is the subscribe facet: fn fires after every Resolve (success or
// tombstone). Multiple subscribers compose; the server registers one per
// linker to start the silent agent_tailer.
type Notifier interface {
	OnResolve(fn func(taskID, toolUseID, internalAgentID string))
}

// Resolver is the lookup facet returning cached link state for a taskID.
type Resolver interface {
	// Query returns the cached LinkInfo without scanning disk; ok=false means
	// "still resolving" (server answers HTTP 202 / WS reason="pending").
	Query(taskID string) (cli.LinkInfo, bool)

	// QueryOrResolveFast returns cached info or runs a single fast-path stat
	// without retries, so a reconnecting tab gets a direct answer in <1ms.
	QueryOrResolveFast(taskID string) (cli.LinkInfo, bool)
}

// PathProvider is the filesystem-anchor facet.
type PathProvider interface {
	// ProjectSessionDir returns <projectDir>/<parentSessionID>, or "" before
	// the init event; the server's path-traversal anchor for tool_result.
	ProjectSessionDir() string
}

// AgentLinker is the subset of *cli.SubagentLinker the server consumes. A
// backend without a linker returns zero-valued cli.LinkInfo and the server
// treats it as tombstone. cli.LinkInfo is reused rather than mirrored:
// server already imports internal/cli, and the decoupling that matters is
// the map-key identity and method set, not the value type.
type AgentLinker interface {
	Notifier
	Resolver
	PathProvider
}
