// Package cli wraps the long-lived backend CLI subprocess
// (Claude Code in stream-json mode, kiro/Gemini via ACP) and turns
// its stdout/stdin pipes into typed Go events the rest of naozhi
// can consume.
//
// # Sub-domains
//
// The package hosts seven loosely-coupled sub-domains in one
// directory; the boundary work tracked by R230B-ARCH-11,
// R226-ARCH-14, R230-ARCH-17, and R230B-ARCH-17 is to lift each
// into its own subpackage. Until that refactor lands, treat this
// godoc as the topographical map a new contributor needs:
//
//   - Process — process.go, process_*.go: the spawn/respawn/Kill
//     state machine, shim attach/detach, stdout/stdin pumps. Owns
//     the Process struct (exported); every other sub-domain hangs
//     off a Process field.
//
//   - Protocol — protocol.go + protocol_claude.go +
//     protocol_acp.go: ReadEvent / Send / Cancel framing per
//     backend. Implements the Protocol interface so Process never
//     hard-codes Claude vs ACP semantics. R234-ARCH-12 calls for
//     a docs/rfc/event-pipeline-contracts.md plus a shared
//     protocoltest harness; until those exist, treat
//     protocol_claude.go as the reference implementation and
//     protocol_acp.go as the kiro/Gemini variant.
//
//   - EventLog — eventlog.go: in-memory ring buffer (default 500
//     entries) the dashboard live-tails. Holds the PersistSink
//     contract (see SetPersistSink godoc for the store-ordering
//     proof). The on-disk persistence engine lives in
//     internal/eventlog/persist; this file is the in-memory
//     half + the cli-only types (EventEntry, SubagentInfo).
//     R230B-ARCH-17 / R226-ARCH-14 track the eventual move of
//     the in-memory layer to internal/eventlog so the schema
//     types are not anchored to a CLI-specific package.
//
//   - SubagentLinker — subagent_link.go +
//     subagent_transcript.go: resolves "internal_agent_id" for
//     Task tool invocations by tailing ~/.claude project JSONLs
//     and matching first-prompt heuristics (RFC v4 §3.5).
//     Holds resolveSem to bound concurrent disk scans.
//
//   - ShimManager — wrapper.go (the public ShimManager field)
//     plus the manager package referenced via wrapper.Wrapper.
//     Backend-aware spawn helper that decides between in-process
//     CLI and the shim transport. The actual shim manager lives
//     in internal/shim; cli.Wrapper is the glue.
//
//   - passthrough — passthrough.go: read-only fixture mode used
//     by tests / replay tools to feed canned stdout into a
//     Process without launching a real CLI.
//
//   - Payload helpers — image.go, thumbnail.go, todo.go, uuid.go:
//     pure helpers for image data-URI sanitisation, base64
//     thumbnails, todo-list parsing, and UUID stamping.
//     R226-ARCH-14 schedules these for internal/cli/payload.
//
// # File-size hot spots
//
// process.go and eventlog.go each exceed 1k lines and host
// multiple sub-domains; the architecture review TODOs above are
// the long-term remediation. Smaller PRs can land focussed
// extractions (e.g. moving image/thumbnail to a payload package)
// without breaking the public surface — Process consumers
// outside this package use the methods on Process, not the
// underlying helpers.
//
// # Public surface
//
// Cross-package callers should depend on the Process struct, the
// Protocol interface, and the EventLog observation API
// (Subscribe / EntriesSince / Snapshot). Internal helpers
// (sanitizeImagesAligned, stampUUID, applyEntryStateLocked,
// resolveSem, …) are unexported and may move freely between
// files inside this directory.
package cli
