// Package cli wraps the long-lived backend CLI subprocess (Claude Code in
// stream-json mode, kiro/Gemini via ACP, codex via app-server) and turns its
// stdout/stdin pipes into typed Go events the rest of naozhi consumes.
//
// # Sub-domains
//
//   - Process — process.go, process_*.go: spawn/respawn/Kill state machine,
//     shim attach/detach, stdout/stdin pumps. Owns the exported Process
//     struct; every other sub-domain hangs off a Process field.
//     process_readloop.go is the stdout NDJSON reader (every ev.recvAt and
//     EventLog AppendBatch originates there); process_send.go is the
//     Send/Interrupt write path; process_turn.go tracks turn boundaries;
//     process_shim_io.go is pure shim framing; process_event_format.go
//     converts Event → EventEntry and formats tool input;
//     process_event_query.go is the read-only EventLog accessor surface.
//   - Protocol — protocol.go + protocol_claude.go / protocol_acp.go /
//     protocol_codex.go: per-backend ReadEvent / Write* framing behind the
//     Protocol interface so Process never hard-codes backend semantics.
//     protocol_claude.go is the reference implementation.
//   - EventLog — eventlog*.go: in-memory ring buffer (default 500 entries)
//     the dashboard live-tails, plus the PersistSink contract. The on-disk
//     writer is internal/eventlog/persist and the replay reader is
//     internal/history/naozhilog; cli.PersistSink ([]EventEntry) and
//     persist.PersistSink (persist.Entry) are deliberately distinct types
//     bridged in session/eventlog_bridge.go. Write path: eventlog_append.go;
//     subagent tracking: eventlog_agents.go; persistence fan-out:
//     eventlog_persist.go; subscribers (subMu, independent of l.mu):
//     eventlog_subscribe.go; read path: eventlog_query.go.
//   - SubagentLinker — subagent_link.go + subagent_transcript.go: resolves
//     internal_agent_id for Task tool calls by tailing ~/.claude project
//     JSONLs; resolveSem bounds concurrent disk scans.
//   - Wrapper / Runner — wrapper.go, runner.go: backend-aware spawn helper
//     over the shim transport (internal/shim) and the Runner placement seam.
//   - passthrough — passthrough.go: uuid/slot-matched concurrent Send path
//     that lets the CLI's own command queue order turns.
//   - Payload helpers — image*.go, thumbnail.go, todo.go, uuid.go.
//
// # Process lifecycle
//
// ProcessState moves StateSpawning → StateReady → StateRunning ⇄ StateReady
// → StateDead. Send moves Ready→Running (a mid-turn shim reconnect may also
// resume into Running); a Type=="result" (or codex turn-end) Event closes the
// turn. Kill/Close/cli_exited move to StateDead and fire onTurnDone, which may
// run more than once per turn and therefore must be idempotent.
//
// # Protocol contract
//
// ReadEvent turns one stdout line into zero or more Events; its done flag is
// advisory and ignored by production callers — turn-end is detected from the
// emitted events, so an implementation MUST emit a result/turn-end Event.
// WriteUserMessageLocked requires the caller to hold Process.shimWMu so the
// sendSlot append and the stdin write are atomic (FIFO slot matching).
//
// # Public surface
//
// Cross-package callers depend on Process, the Protocol interface, and the
// EventLog observation API (Subscribe / EntriesSince / LastN). Unexported
// helpers may move freely between files in this directory.
package cli
