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
//     # "eventlog" is three packages, not one (R237-ARCH-13)
//
//     The token "eventlog" appears in three distinct positions
//     of the data flow; misreading them as one package causes
//     diff confusion in reviews. Disambiguation:
//
//   - cli.EventLog (this package, eventlog.go) — IN-MEMORY
//     ring buffer + PersistSink contract. Producer of every
//     event. Owns EventEntry / SubagentInfo / PersistSink.
//
//   - internal/eventlog/persist — ON-DISK writer (per-key
//     <stem>.log + <stem>.idx + framing + rotate). Consumes
//     from cli.EventLog via the PersistSink closure. Owns
//     Persister / Recover / IdxWriter.
//
//   - internal/eventlog/schema — wire format types
//     (Record / FileHeader / IdxEntry) shared by persist
//
//   - future replay readers. NEVER imports cli.
//
//   - internal/history/naozhilog — REPLAY reader. Mounts
//     the on-disk files persist wrote and exposes them to
//     the history merge layer.
//
//     R237-ARCH-13 proposes the long-term rename to
//     internal/eventlog/{ring, persist, replay} so the package
//     names match the data-flow positions. Until that lands,
//     reviewers MUST NOT collapse references — a "PersistSink"
//     in cli (cli.PersistSink takes []EventEntry) and a
//     "PersistSink" in persist (persist.PersistSink takes
//     persist.Entry) are deliberately distinct types; the
//     bridge in session/eventlog_bridge.go translates between
//     them. See PersistSink godoc in eventlog_persist.go for
//     the full ARCH-4 anchor.
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
// The two former god-files have been split by responsibility:
// process.go (ARCH-PROCESS-SPLIT) into 6 process_*.go files and
// eventlog.go (ARCH-EVENTLOG-SPLIT) into 6 eventlog_*.go files —
// see the per-group file maps below. Each owning file now sits
// well under 1k lines. Further extractions (e.g. moving
// image/thumbnail to a payload package) can land as focussed PRs
// without breaking the public surface — Process consumers outside
// this package use the methods on Process, not the underlying
// helpers.
//
// # process_*.go file map (R243-ARCH-21)
//
// The Process state machine is split across 7 non-test
// process_*.go files; a 2026-04 review (R243-ARCH-21,
// REPEAT-3) flagged this as "拆得过细" and noted stale
// "Deprecated" / TODO anchors that no longer reflected current
// content. The split is preserved (each file owns a coherent
// slice of the type's methods, total ~5.5 kLoC) but reviewers
// MUST treat the per-file headers as authoritative, not the
// filename token alone:
//
//   - process.go — struct, lifecycle constants, ProcessState
//     enum, Spawn entry-point, sentinel errors. Owns the type
//     declaration; everything else hangs off methods on
//     *Process.
//   - process_readloop.go — stdout NDJSON reader goroutine,
//     event coalescing, watchdog timers. The hottest single
//     file: every ev.recvAt assignment + EventLog AppendBatch
//     comes from here.
//   - process_send.go — Send() / Cancel() write path; user
//     turn entry buffering; image attachment marshalling.
//   - process_turn.go — turn boundary tracking + replay
//     bookkeeping (turn IDs, optimistic-bubble dedup).
//   - process_shim_io.go — shim transport framing helpers
//     (shimWriter / shimLineReader); pure I/O, no semantics.
//   - process_event_format.go — Event → EventEntry pure
//     conversion. Earlier "Deprecated" header noise has been
//     cleared (R243-ARCH-21); the EventEntriesFromEvent
//     test-helper variant was retired in DEADCODE-7 (only
//     EventEntriesFromEventAt remains). The orthogonal
//     tool-input formatting concern was split out to
//     process_tool_format.go (#866) so this file owns only the
//     conversion slice.
//   - process_tool_format.go — pure tool-input → label
//     formatting (FormatToolInput, parseAgentInput, shortPath
//     and the per-tool input shapes). No *Process dependency;
//     the "payload/format" slice extracted per #866.
//   - process_event_query.go — read-only EventLog accessors
//     (EventEntries / EventLastN / EventEntriesSince /
//     EventEntriesBefore) + Linker lifecycle + InjectHistory.
//
// The eventual refactor — splitting eventbus / linker / payload
// into subpackages — is tracked separately on R243-ARCH-21 and
// requires breaking the *Process method-set up; until that
// lands, treat this map as the topological anchor and prefer
// localised godoc patches (this commit) over partial extractions
// that would leave method receivers split across multiple
// packages mid-refactor.
//
// # eventlog_*.go file map (ARCH-EVENTLOG-SPLIT)
//
// The in-memory EventLog is split across 6 non-test eventlog_*.go
// files (move-only, zero semantic change; see
// docs/rfc/eventlog-split.md). As with process_*.go, reviewers
// MUST treat the per-file headers as authoritative:
//
//   - eventlog.go — EventLog struct, ring-buffer constants, the
//     EventEntry alias, and NewEventLog. Owns the type
//     declaration; everything else hangs off methods on
//     *EventLog.
//   - eventlog_append.go — write path: Append / AppendBatch,
//     ring-buffer eviction, summary-cache + atomic-counter
//     updates, and the image-sanitize helper.
//   - eventlog_agents.go — per-turn subagent tracking
//     (applyEntryStateLocked, the O(1) taskIndex / toolUseIndex /
//     agentRingByToolUse sidecars), task_done callbacks, and the
//     TurnAgents / Subagents / BgSubagents accessors.
//   - eventlog_persist.go — the PersistSink / PersistSinkOne
//     contract, SetPersistSink(Pair), the invoke fan-out, and the
//     replay-phase guard atomics.
//   - eventlog_subscribe.go — subscriber broadcast (Subscribe /
//     SubscribeNew / notifySubscribers / CloseSubscribers) plus
//     the EventSubscription handle, guarded by subMu independently
//     of the ring-buffer l.mu.
//   - eventlog_query.go — read path: Entries / LastN /
//     EntriesSince / EntriesBefore (and their *Append
//     buffer-reuse variants), Count, and the lock-free summary
//     accessors.
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
