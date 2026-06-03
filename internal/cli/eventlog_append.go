// File eventlog_append.go: the EventLog write path — Append / AppendBatch, ring-buffer eviction,
// summary-cache + atomic-counter updates, and the image-sanitize helper.
// Split from eventlog.go per docs/rfc/eventlog-split.md (ARCH-EVENTLOG-SPLIT);
// the EventLog struct and constructor live in eventlog.go.

package cli

import (
	"strings"
	"time"
)

// sanitizeImagesAligned drops any data URI that is not an image/* data URL
// and strips empty strings so a single skipped thumbnail does not leave a
// "" slot the dashboard would have to render defensively. Returns the input
// slice unchanged when every entry is already valid, avoiding an allocation
// on the happy path (MakeThumbnail conforming producer).
//
// paths is an optional index-aligned slice of workspace-relative paths
// (EventEntry.ImagePaths) that MUST be filtered in lock-step so the
// dashboard's click-thumbnail-for-original flow stays aligned with the
// thumbnail it drew. Pass nil when the caller has no paths. The returned
// filtered paths slice is nil when every Images entry was valid (no
// allocation) OR when every path was dropped.
//
// Two-pass design (R229-PERF-8): the first pass is a pure read scan that
// short-circuits on the first invalid entry and returns the inputs untouched
// when every URI is well-formed — the common case under MakeThumbnail. The
// second pass only runs when the fast path failed and is the only place that
// allocates (`filtered` and `filteredPaths`). Folding the two passes into a
// single allocate-then-fill loop would force the happy path (every Append +
// AppendBatch with images) to pay one slice allocation per call even when
// nothing needs filtering, defeating the "happy path is zero-alloc" invariant
// that justifies the redundant scan. Do NOT collapse into one loop without
// re-running the bench at internal/cli/eventlog_images_align_test.go.
func sanitizeImagesAligned(imgs, paths []string) ([]string, []string) {
	if len(imgs) == 0 {
		return imgs, nil
	}
	allOK := true
	for _, s := range imgs {
		if s == "" || !strings.HasPrefix(s, imageDataURIPrefix) {
			allOK = false
			break
		}
	}
	if allOK {
		return imgs, paths
	}
	filtered := make([]string, 0, len(imgs))
	var filteredPaths []string
	if len(paths) > 0 {
		filteredPaths = make([]string, 0, len(imgs))
	}
	anyPath := false
	for i, s := range imgs {
		if s == "" || !strings.HasPrefix(s, imageDataURIPrefix) {
			continue
		}
		filtered = append(filtered, s)
		// Lock-step append to filteredPaths — NEVER skip an append when
		// `paths` is non-empty, otherwise `filtered[j]` stops matching
		// `filteredPaths[j]` and a dashboard thumbnail click could fetch
		// the bytes of a DIFFERENT image in the same message. The gate is
		// "filteredPaths was initialised", not "i < len(paths)": replayed
		// history (AppendBatch/InjectHistory) can feed untrusted
		// EventEntry values where len(ImagePaths) < len(Images); pad with
		// "" so the lightbox degrades to the thumbnail for that slot
		// instead of serving a sibling image.
		if filteredPaths != nil {
			var p string
			if i < len(paths) {
				p = paths[i]
			}
			filteredPaths = append(filteredPaths, p)
			if p != "" {
				anyPath = true
			}
		}
	}
	if len(filtered) == 0 {
		return nil, nil
	}
	if !anyPath {
		filteredPaths = nil
	}
	return filtered, filteredPaths
}

// stampUUID guarantees every appended EventEntry has a non-empty
// UUID. Legacy callers that already set UUID (e.g. history replay
// paths using textutil.DeriveLegacyUUID for determinism) keep their
// value; everything else gets a fresh newEventUUID.
//
// Called from Append / AppendBatch inside the l.mu write-lock so
// the ring buffer always stores the definitive UUID downstream
// readers (Entries, EntriesSince, EntriesBefore, invokePersistSink)
// see.
func stampUUID(e *EventEntry) {
	if e.UUID == "" {
		e.UUID = newEventUUID()
	}
}

// Append adds an entry to the log, overwriting the oldest entry when full.
// Signals all subscribers non-blockingly after appending.
func (l *EventLog) Append(e EventEntry) {
	// R225-PERF-11: stamp UUID *before* taking l.mu — newEventUUID calls
	// crypto/rand.Read which on Linux is a getrandom() syscall. Holding l.mu
	// across that syscall serialises every concurrent Append behind the
	// kernel entropy pool and bloats lock-hold time on the 5-50 events/s
	// per-session hot path. stampUUID is a pure function of the entry's UUID
	// field — caller-set UUIDs (history replay paths) are preserved unchanged.
	stampUUID(&e)
	l.mu.Lock()
	// Single time.Now() feeds both the event timestamp (if absent) and the
	// lastEventAt heartbeat below. Both reads used to happen independently
	// causing two vDSO calls per Append on the hot path. The tiny skew
	// between the two was meaningless — Cleanup only needs "some event
	// landed recently", and the entry's own Time already represents the
	// "actually received at" moment.
	now := time.Now()
	if e.Time == 0 {
		e.Time = now.UnixMilli()
	}
	// Server-side enforcement that every image entry is a data:image/* URI.
	// Today's sole producer (MakeThumbnail) already conforms, but enforcing
	// the contract here rather than trusting callers means any future
	// producer that accidentally passes through an external URL or a
	// javascript: URI gets stripped before it can reach the dashboard's
	// <img src=...> render path. S15 (Round 174).
	//
	// Fast-path skip: 99%+ of events carry no images; hoist the len check
	// to avoid the function call overhead on every live append.
	if len(e.Images) > 0 {
		e.Images, e.ImagePaths = sanitizeImagesAligned(e.Images, e.ImagePaths)
	}
	ringIdx := l.head
	l.entries[l.head] = e
	l.head = (l.head + 1) % l.maxSize
	if l.count < l.maxSize {
		l.count++
	}
	// R260528-PERF-22 (#1360): pin the ring slot for agent/task_start so
	// the linker's OnResolve callback can hop straight here without the
	// 50-slot reverse scan under wlock. Cheap no-op for the >99% of
	// entries whose Type is neither "agent" nor "task_start".
	l.recordAgentRingPosLocked(e.Type, e.ToolUseID, ringIdx)

	// Skip the switch dispatch + per-call frame for entry types that
	// fall through applyEntryStateLocked's default arm with zero work
	// (assistant_text, tool_use, tool_result, system, …). R240-PERF-3.
	var (
		firePending bool
		pending     pendingTaskDone
	)
	if entryAffectsAgentState(e.Type) {
		firePending, pending = l.applyEntryStateLocked(e)
	}

	// Atomic summary stores are issued *inside* l.mu so that AppendBatch,
	// which holds l.mu for its full duration, cannot have its later Store
	// racing with a concurrent live Append's Store — the serialization on
	// l.mu guarantees last-writer-wins matches entry-order, not
	// entry-ordering-inverted by lock release scheduling.
	if e.Type == "user" {
		storeAtomicString(&l.lastPromptSummary, e.Summary)
		l.userTurnCount.Add(1)
	} else if e.Type == "text" {
		// R110-P1: assistant text reply — feed sidebar 30-rune preview.
		// Stored even when Summary is empty so a freshly-streamed empty
		// text block (rare but possible) overwrites stale values rather
		// than leaving last-turn's response visible.
		storeAtomicString(&l.lastResponseSummary, e.Summary)
	} else if IsActivityType(e.Type) {
		// IsActivityType is the shared predicate for the "activity" set;
		// session.ManagedSession history scans also consume it so the
		// live-tail and replay-tail stay aligned. R228-CR-3.
		storeAtomicString(&l.lastActivitySummary, e.Summary)
	}

	// Record live-activity timestamp. A single Store is fine: Cleanup only
	// cares about "some event landed recently", and later Appends overwrite
	// with a never-decreasing value. Reuses the `now` captured at function
	// entry — one vDSO call per Append instead of two.
	l.lastEventAt.Store(now.UnixNano())

	l.mu.Unlock()

	// Fire task_done callbacks OUTSIDE l.mu so a slow subscriber (e.g. the
	// server tailer registry closing N tailers) cannot serialise concurrent
	// Append calls or wedge the ring buffer mid-write. R201-CRIT-1.
	//
	// R224-PERF-1: use the single-entry fast path so the one-slot slice
	// literal `[]pendingTaskDone{pending}` does not heap-escape on every
	// task_done event. AppendBatch still calls the slice form below.
	if firePending {
		l.fireOneTaskDoneCallback(pending)
	}

	// Invoke persistence sink OUTSIDE l.mu. Passing a fresh one-slot
	// slice matches PersistSink's retention contract (callers may hold
	// the slice past return). The slice copy is O(1) because len=1.
	//
	// R230-PERF-1: skip the slice literal entirely when no sink is
	// wired (test harnesses, headless tools, the InjectHistory replay
	// phase before the persister attaches). The slice header + the
	// `EventEntry` copy together heap-escape on every Append; bypassing
	// them in the no-sink case saves one alloc per event in the hot
	// stdout path. Mirrors AppendBatch's pre-loop sinkAttached gate.
	//
	// #410: prefer the single-entry sink when the caller paired one via
	// SetPersistSinkPair. The single sink path passes the EventEntry by
	// value, eliminating the `[]EventEntry{e}` literal that would
	// otherwise heap-escape through the slice sink's retention contract.
	// Falls back to the slice form for legacy SetPersistSink-only
	// callers; both branches share replayPhase derivation +
	// replayInvokeTotal accounting through invokePersistSinkOne /
	// invokePersistSink.
	//
	// R215-PERF-P2-1 / R219-PERF-4 / R228-PERF-7 archive anchor:
	// the slice-literal allocation on the legacy sink-attached branch
	// remains structurally required by PersistSink's retention
	// contract — opting into SetPersistSinkPair is the way to skip it.
	if !l.invokePersistSinkOne(e) {
		if l.persistSinkPtr.Load() != nil {
			l.invokePersistSink([]EventEntry{e})
		}
	}

	l.notifySubscribers()
}

// AppendBatch adds multiple entries to the log, holding the lock once and
// notifying subscribers once. Used by live dispatch (multi-block assistant
// events) to avoid per-entry lock acquisition + subscriber wake-ups.
//
// Mirrors Append's per-entry sub-agent tracking and summary atomics so the
// sidebar does not show stale "(no prompt)" placeholders after history
// injection until a live event arrives. Atomic summary writes happen under
// l.mu to avoid a race with concurrent Append: if a live event ran Store
// after our Unlock but before our own Store, our older batch value would
// clobber it.
func (l *EventLog) AppendBatch(entries []EventEntry) {
	l.appendBatch(entries, false)
}

// AppendBatchReplay is the replay-aware variant used by InjectHistory.
// Setting isReplay=true skips applyEntryStateLocked entirely: replay never
// triggers the on-task-done callback (the persister is not yet wired and
// downstream tailers don't yet exist), so the per-entry switch dispatch +
// turn/bg agent slice scans inside l.mu are pure overhead. R240-PERF-3
// (#1042). Live AppendBatch callers MUST keep isReplay=false so task_done
// callbacks continue to fire on real turn-end events.
func (l *EventLog) AppendBatchReplay(entries []EventEntry) {
	l.appendBatch(entries, true)
}

func (l *EventLog) appendBatch(entries []EventEntry, isReplay bool) {
	if len(entries) == 0 {
		return
	}
	var (
		lastPrompt, lastActivity, lastResponse string
		sawPrompt, sawActivity, sawResponse    bool
		userDelta                              int64
		pendingDone                            []pendingTaskDone
	)
	// Capture a single wall-clock read before locking so the N zero-time
	// entries inside the loop (typical case: InjectHistory's 500-entry
	// replay on shim reconnect) don't each fire a vDSO call under l.mu.
	// Correctness: entries with an explicit Time are unaffected; entries
	// without one are assigned a monotonically-close "now" that is as
	// semantically correct as the per-entry reads they replace, while
	// keeping the write-lock hold time bounded. R71-PERF-L2.
	defaultTime := time.Now().UnixMilli()
	// Allocate the sink-copy slice outside the lock so the write
	// lock hold time is bounded by the ring write itself. The slice
	// is populated inside the loop and handed to invokePersistSink
	// after unlock.
	//
	// Fast path: when no persist sink is wired we skip the per-batch
	// allocation entirely. invokePersistSink does the same nil check at
	// :337 but only after we've already paid for the slice; routers
	// without a sink (test harnesses, headless tools, the InjectHistory
	// replay path before the persister is attached) hit this branch on
	// every batch and a 500-slot allocation per replay adds up. Read the
	// sink pointer once here so the body and the post-unlock dispatch
	// agree on whether to capture; a Store racing this read is fine —
	// the late-attached sink will pick up subsequent batches and the
	// missed ones are bounded by the same replayPhase contract that
	// already gates the early append phase.
	// R242-PERF-8: skip the sinkCopy allocation entirely when the sink
	// observes !sinkReady (replay phase). The persister sink unconditionally
	// drops replay-phase batches (see Persister.SinkFor → replayLeakCnt path),
	// so capturing entries we know will be discarded just burns heap on every
	// InjectHistory's 500-entry replay round-trip.
	//
	// Read order: ptr first, then sinkReady. If a SetPersistSink races between
	// our two loads we'll observe (ptr=non-nil, ready=false) → still skip;
	// the batch is genuinely replayPhase from the contract's POV because
	// SetPersistSink writes ptr first and ready second. The next batch after
	// SetPersistSink completes will see ready=true and allocate normally.
	//
	// `sinkAttached` covers the historical fast-path (no sink wired at all,
	// e.g. test harnesses); `captureForSink` is the per-batch decision used
	// to gate both the allocation above and the in-loop append below — they
	// must agree, otherwise the loop would append into a nil slice or the
	// post-unlock dispatch would receive an empty copy from a non-replay
	// batch.
	// R20260530-COR-1 (#1482): replay-phase batches (AppendBatchReplay,
	// InjectHistory) must NEVER be captured into sinkCopy nor pushed to the
	// persist sink. The persister only drops them while sinkReady==false; if
	// a reattach flips sinkReady=true *before* a late InjectHistory runs
	// (reconnect/reattach ordering), every replayed historical entry would be
	// re-persisted, duplicating already-written JSONL turns. Gate on
	// !isReplay here so the replay contract holds regardless of sinkReady —
	// this mirrors the unconditional applyEntryStateLocked skip at :398.
	sinkAttached := !isReplay && l.persistSinkPtr.Load() != nil
	captureForSink := sinkAttached && l.sinkReady.Load()
	// R225-PERF-11 + R249-PERF-16: single pre-lock pass that stamps UUIDs,
	// applies the default Time, and sanitises image URIs. The N
	// crypto/rand.Read syscalls (getrandom, one per missing UUID) and the
	// ~200KB sinkCopy build for a 500-entry InjectHistory replay all happen
	// outside the write-lock. Caller-set UUIDs (history replay) are preserved
	// by stampUUID's no-op-on-non-empty contract.
	//
	// `sinkCopy` doubles as the inner-loop iteration source so per-entry
	// stamping (default time, image sanitize) is also paid only once per
	// entry — the ring-buffer write inside the lock simply assigns the
	// pre-prepared struct without re-running sanitize/default-time logic.
	//
	// Fast path (!captureForSink): sinkCopy stays nil, but the per-entry
	// preprocessing (default Time + image sanitize) still runs here in a
	// `prepared` buffer so the write-lock section only assigns pre-built
	// structs. R103901-PERF-7: the prior shape left the !captureForSink
	// branch running sanitizeImagesAligned (a string scan) + the Time
	// assignment *inside* l.mu, bloating lock-hold time for test harnesses
	// and the InjectHistory replay phase before the persister attaches —
	// exactly the 500-entry replay hot path the captureForSink branch was
	// already optimised for. We mirror that optimisation here. The prepared
	// buffer (rather than mutating `entries[i]` in place) preserves the
	// historical contract that the caller's slice is untouched beyond the
	// UUID stamp that stampUUID already applies in place.
	// R20260602190132-PERF-3: len==1 fast path for the captureForSink branch.
	// AppendBatch is frequently called with a single entry (e.g. live
	// dispatch of one-block assistant events). make([]EventEntry, 1) for
	// sinkCopy allocates a heap slice even though the entry will be passed
	// to the persist sink immediately after unlock. When len==1 and
	// captureForSink is true we instead use a stack-allocated EventEntry
	// (sinkOne) and route through invokePersistSinkOne (pass-by-value, zero
	// slice alloc) or fall back to []EventEntry{sinkOne} when only the
	// batch sink is wired. sinkCopy stays nil for this path.
	//
	// The historical contract from comments 342-344 is fully preserved:
	// stampUUID(&entries[i]) still runs in-place on the caller's slice for
	// every entry before e := entries[i] copies the value — only the
	// prepared/sinkCopy destination changes for the len==1 path.
	var (
		sinkCopy   []EventEntry
		prepared   []EventEntry
		sinkOne    EventEntry
		sinkOneSet bool
	)
	if captureForSink {
		if len(entries) == 1 {
			sinkOneSet = true // use sinkOne scalar; sinkCopy stays nil
		} else {
			sinkCopy = make([]EventEntry, len(entries))
		}
	} else {
		prepared = make([]EventEntry, len(entries))
	}
	for i := range entries {
		// R20260602190132-PERF-12: stamp the UUID in place on the caller's
		// slice (historical contract, see comments above), then copy the
		// stamped entry ONCE into its prepared destination and apply the
		// default-Time + image-sanitize preprocessing through a pointer to
		// that slot. The prior shape did `e := entries[i]` into a loop local
		// and then `dest[i] = e`, a second full EventEntry struct copy
		// (~240 B) per entry on top of the in-place stamp.
		stampUUID(&entries[i])
		var dst *EventEntry
		if sinkOneSet {
			sinkOne = entries[i]
			dst = &sinkOne
		} else if captureForSink {
			sinkCopy[i] = entries[i]
			dst = &sinkCopy[i]
		} else {
			prepared[i] = entries[i]
			dst = &prepared[i]
		}
		if dst.Time == 0 {
			dst.Time = defaultTime
		}
		// S15 (Round 174): same enforcement as Append. Replays from
		// history (InjectHistory → AppendBatch) should never contain
		// non-image data URIs today, but defense-in-depth is trivially
		// cheap and locks the contract to a single sink.
		if len(dst.Images) > 0 {
			dst.Images, dst.ImagePaths = sanitizeImagesAligned(dst.Images, dst.ImagePaths)
		}
	}
	l.mu.Lock()
	for idx := range entries {
		// R20260527122801-PERF-7 + R103901-PERF-7: both branches have
		// already fully prepared the entry (UUID stamp, default Time, image
		// sanitize) in the pre-lock loop — sinkCopy[idx] when capturing for
		// the persist sink, prepared[idx] otherwise. Write it directly into
		// the ring slot and rebind ePtr to that slot so the downstream
		// state-tracking code reads from the canonical store without paying a
		// second per-entry struct copy through a range-loop local, and
		// without running sanitizeImagesAligned (a string scan) inside l.mu.
		var ePtr *EventEntry
		if sinkOneSet {
			// len==1 fast path: sinkOne holds the pre-prepared entry;
			// sinkCopy is nil (allocation skipped). R20260602190132-PERF-3.
			l.entries[l.head] = sinkOne
		} else if captureForSink {
			l.entries[l.head] = sinkCopy[idx]
		} else {
			l.entries[l.head] = prepared[idx]
		}
		ePtr = &l.entries[l.head]
		ringIdx := l.head
		l.head = (l.head + 1) % l.maxSize
		if l.count < l.maxSize {
			l.count++
		}
		// R260528-PERF-22 (#1360): pin agent/task_start ring slots so
		// the SubagentLinker's OnResolve can backfill in O(1). Recorded
		// on the replay path too — InjectHistory replays agent/task_start
		// entries that may still be linker-pending after shim reconnect,
		// and the next live SetAgentInternalID call needs to reach them.
		//
		// R20260601-PERF-9 (#1549): gate the call inline. A 500-entry
		// InjectHistory replay is dominated by assistant_text/tool_use rows;
		// recordAgentRingPosLocked early-returns for those, but the
		// unconditional call still costs 500 function-call frames + the
		// type compare under l.mu, stalling concurrent Append (readLoop hot
		// path). Hoisting the agent/task_start test out front lets the
		// common case skip the call entirely while preserving the replay
		// sidecar contract for the rare agent/task_start rows. The map is
		// only mutable under l.mu, so the write must stay inside the lock —
		// only the now-rare entries pay for it.
		if ePtr.Type == "agent" || ePtr.Type == "task_start" {
			l.recordAgentRingPosLocked(ePtr.Type, ePtr.ToolUseID, ringIdx)
		}

		// Skip applyEntryStateLocked for entries whose Type is not one of
		// the 6 cases the function actually handles. InjectHistory's
		// 500-entry replay is dominated by assistant_text/tool_use rows
		// which previously paid switch-dispatch + return overhead inside
		// the write lock. R240-PERF-3.
		//
		// On the replay path we skip applyEntryStateLocked unconditionally:
		// no on-task-done subscriber is wired during InjectHistory (#1042),
		// and the turnAgents/bgAgents per-turn slices are reset by the
		// next live "result"/"user" event anyway. This avoids 500× O(N)
		// agent-slice scans inside the write-lock during shim reconnect.
		if !isReplay && entryAffectsAgentState(ePtr.Type) {
			if fire, p := l.applyEntryStateLocked(*ePtr); fire {
				pendingDone = append(pendingDone, p)
			}
		}

		// Track last-of-kind summaries so a single Store (below, still
		// under l.mu) captures the tail of the batch. The "saw" flag is
		// separate from the value so an empty final Summary still
		// overwrites the atomic — Append stores unconditionally for these
		// types, and diverging here would leave stale summaries visible.
		if ePtr.Type == "user" {
			lastPrompt = ePtr.Summary
			sawPrompt = true
			userDelta++
		} else if ePtr.Type == "text" {
			// R110-P1: track tail assistant text for sidebar response preview.
			// Mirrors the live Append store under l.mu — last-writer-wins
			// matches entry order even when batches interleave with live
			// Appends. See lastPromptSummary single-Store treatment above.
			lastResponse = ePtr.Summary
			sawResponse = true
		} else if IsActivityType(ePtr.Type) {
			lastActivity = ePtr.Summary
			sawActivity = true
		}
	}

	if sawPrompt {
		storeAtomicString(&l.lastPromptSummary, lastPrompt)
	}
	if sawResponse {
		storeAtomicString(&l.lastResponseSummary, lastResponse)
	}
	if sawActivity {
		storeAtomicString(&l.lastActivitySummary, lastActivity)
	}
	if userDelta > 0 {
		// Single atomic add mirrors the lastPromptSummary single Store above —
		// callers observe the batch's cumulative impact in one step. Under l.mu
		// so the count is seen by any concurrent Snapshot that also reads
		// other per-turn state.
		l.userTurnCount.Add(userDelta)
	}
	l.mu.Unlock()

	l.fireTaskDoneCallbacks(pendingDone)

	// Invoke persistence sink outside l.mu. sinkCopy holds the
	// post-stamp, post-sanitize entries in the SAME order they were
	// committed to the ring buffer — critical for the Persister's
	// write-order guarantees.
	//
	// R20260530-COR-1 (#1482): replay batches never feed the persist sink
	// (sinkCopy is nil because captureForSink gates on !isReplay above).
	// Skip the call outright so a late InjectHistory cannot re-persist
	// already-written history even if sinkReady has already flipped true.
	//
	// R20260602190132-PERF-3: sinkOneSet is the len==1 captureForSink fast
	// path. Try invokePersistSinkOne first (pass-by-value, zero slice alloc);
	// fall back to the slice form only when the batch-only sink is wired.
	if !isReplay {
		if sinkOneSet {
			if !l.invokePersistSinkOne(sinkOne) {
				l.invokePersistSink([]EventEntry{sinkOne})
			}
		} else {
			l.invokePersistSink(sinkCopy)
		}
	}

	l.notifySubscribers()
}
