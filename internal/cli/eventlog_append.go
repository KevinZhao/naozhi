// File eventlog_append.go: the EventLog write path — Append / AppendBatch,
// ring-buffer eviction, summary-cache + atomic-counter updates, and the
// image-sanitize helper.

package cli

import (
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/textutil"
)

// sanitizeImagesAligned drops every entry that is not an image/* data URI and
// strips empty strings. paths (EventEntry.ImagePaths) is filtered in lock-step
// so a thumbnail click keeps fetching its own original; pass nil when absent.
// Returns the inputs unchanged when every entry is valid (zero-alloc happy
// path) and nil paths when every path was dropped.
//
// Two passes on purpose: the read-only scan short-circuits on the common
// all-valid case, and only the second pass allocates. Do NOT collapse into
// one allocate-then-fill loop without re-running the bench in
// eventlog_images_align_test.go.
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
		// Lock-step append: filtered[j] must always match filteredPaths[j] or a
		// thumbnail click could serve a sibling image. Replayed history may
		// carry len(ImagePaths) < len(Images), so pad with "" (lightbox
		// degrades to the thumbnail) rather than skipping the append.
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

// stampUUID guarantees every appended EventEntry has a non-empty UUID.
// Caller-set UUIDs (history replay via textutil.DeriveLegacyUUID) are kept;
// everything else gets a fresh newEventUUID.
func stampUUID(e *clievent.EventEntry) {
	if e.UUID == "" {
		e.UUID = newEventUUID()
	}
}

// Append adds an entry to the log, overwriting the oldest entry when full.
// Signals all subscribers non-blockingly after appending.
func (l *EventLog) Append(e clievent.EventEntry) {
	// Stamp the UUID before taking l.mu: newEventUUID does a getrandom()
	// syscall that must not extend the lock hold on the hot path.
	stampUUID(&e)
	l.mu.Lock()
	// One time.Now() feeds both the default Time and the lastEventAt heartbeat.
	now := time.Now()
	if e.Time == 0 {
		e.Time = now.UnixMilli()
	}
	// Enforce the data:image/* contract here rather than trusting producers so
	// an external or javascript: URI never reaches the dashboard <img src>.
	if len(e.Images) > 0 {
		e.Images, e.ImagePaths = sanitizeImagesAligned(e.Images, e.ImagePaths)
	}
	ringIdx := l.head
	l.entries[l.head] = e
	l.head = (l.head + 1) % l.maxSize
	if l.count < l.maxSize {
		l.count++
		l.countAtomic.Add(1)
	}
	// Pin the ring slot for agent/task_start so the linker's OnResolve can
	// backfill in O(1); no-op for every other Type.
	l.recordAgentRingPosLocked(e.Type, e.ToolUseID, ringIdx)

	// Skip the switch dispatch for types applyEntryStateLocked ignores.
	var (
		firePending bool
		pending     pendingTaskDone
	)
	if entryAffectsAgentState(e.Type) {
		firePending, pending = l.applyEntryStateLocked(e)
	}

	// Summary Stores happen inside l.mu so a concurrent AppendBatch (which
	// holds l.mu for its whole run) cannot invert last-writer order.
	if e.Type == "user" {
		storeAtomicString(&l.lastPromptSummary, e.Summary)
		l.userTurnCount.Add(1)
	} else if e.Type == "text" {
		// Stored even when Summary is empty so a fresh empty text block
		// overwrites last turn's preview. Markdown is stripped because the
		// sidebar renders plain text (#2435).
		storeAtomicString(&l.lastResponseSummary, textutil.StripMarkdown(e.Summary))
	} else if IsActivityType(e.Type) {
		// Shared predicate with session history scans so live and replay tails agree.
		storeAtomicString(&l.lastActivitySummary, e.Summary)
	}

	// Never-decreasing heartbeat for Cleanup's "some event landed recently".
	l.lastEventAt.Store(now.UnixNano())

	l.mu.Unlock()

	// task_done callbacks fire OUTSIDE l.mu so a slow subscriber cannot wedge
	// concurrent Appends; single-entry form avoids a heap-escaping slice.
	if firePending {
		l.fireOneTaskDoneCallback(pending)
	}

	// Persistence sink fires OUTSIDE l.mu. Prefer the single-entry sink (no
	// `[]EventEntry{e}` literal, #410); the slice form's fresh one-slot copy
	// honours PersistSink's retention contract. Skip entirely when no sink is
	// wired so the no-persistence path stays alloc-free.
	if !l.invokePersistSinkOne(e) {
		if l.persistSinkPtr.Load() != nil {
			l.invokePersistSink([]clievent.EventEntry{e})
		}
	}

	l.notifySubscribers()
}

// AppendBatch adds multiple entries to the log, holding the lock once and
// notifying subscribers once. Mirrors Append's per-entry sub-agent tracking
// and summary atomics (stored under l.mu so a concurrent live Append cannot be
// clobbered by an older batch value).
func (l *EventLog) AppendBatch(entries []clievent.EventEntry) {
	l.appendBatch(entries, false)
}

// AppendBatchReplay is the replay-aware variant used by InjectHistory. It
// skips applyEntryStateLocked: no task_done subscriber exists during replay
// and the per-turn agent slices reset on the next live result/user anyway
// (#1042). Live callers MUST use AppendBatch so task_done callbacks fire.
func (l *EventLog) AppendBatchReplay(entries []clievent.EventEntry) {
	l.appendBatch(entries, true)
}

func (l *EventLog) appendBatch(entries []clievent.EventEntry, isReplay bool) {
	if len(entries) == 0 {
		return
	}
	var (
		lastPrompt, lastActivity, lastResponse string
		sawPrompt, sawActivity, sawResponse    bool
		userDelta                              int64
		pendingDone                            []pendingTaskDone
	)
	// One wall-clock read for every zero-Time entry keeps vDSO calls out of
	// the write lock on 500-entry replays.
	defaultTime := time.Now().UnixMilli()
	// Replay batches (AppendBatchReplay / InjectHistory) must NEVER reach the
	// persist sink: if a reattach flips sinkReady before a late InjectHistory,
	// every replayed entry would be re-persisted (#1482). Read ptr before
	// sinkReady: a racing SetPersistSink stores ptr first, so observing
	// (ptr!=nil, ready=false) is still correctly a replay-phase batch.
	sinkAttached := !isReplay && l.persistSinkPtr.Load() != nil
	captureForSink := sinkAttached && l.sinkReady.Load()
	// All per-entry preprocessing (UUID stamp, default Time, image sanitize)
	// runs before the lock into a prepared destination so the locked section
	// only assigns pre-built structs. len==1 batches use a stack scalar
	// (sinkOne / preparedOne) instead of a heap slice; the caller's slice is
	// only touched by the in-place UUID stamp.
	var (
		sinkCopy       []clievent.EventEntry
		prepared       []clievent.EventEntry
		sinkOne        clievent.EventEntry
		sinkOneSet     bool
		preparedOne    clievent.EventEntry
		preparedOneSet bool
	)
	if captureForSink {
		if len(entries) == 1 {
			sinkOneSet = true // use sinkOne scalar; sinkCopy stays nil
		} else {
			sinkCopy = make([]clievent.EventEntry, len(entries))
		}
	} else if len(entries) == 1 {
		preparedOneSet = true
	} else {
		prepared = make([]clievent.EventEntry, len(entries))
	}
	for i := range entries {
		// Stamp in place on the caller's slice, then copy ONCE into the
		// destination and preprocess through a pointer to that slot.
		stampUUID(&entries[i])
		var dst *clievent.EventEntry
		if sinkOneSet {
			sinkOne = entries[i]
			dst = &sinkOne
		} else if preparedOneSet {
			preparedOne = entries[i]
			dst = &preparedOne
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
		// Same data:image/* enforcement as Append; replays are untrusted too.
		if len(dst.Images) > 0 {
			dst.Images, dst.ImagePaths = sanitizeImagesAligned(dst.Images, dst.ImagePaths)
		}
	}
	l.mu.Lock()
	for idx := range entries {
		// Write the pre-prepared entry straight into the ring slot and read
		// state-tracking fields from the canonical store (no second copy).
		var ePtr *clievent.EventEntry
		if sinkOneSet {
			l.entries[l.head] = sinkOne
		} else if preparedOneSet {
			l.entries[l.head] = preparedOne
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
			l.countAtomic.Add(1)
		}
		// Pin agent/task_start ring slots for the linker, on replay too:
		// replayed entries may still be linker-pending after reconnect. The
		// inline type gate keeps the call off the 500-entry common path; the
		// map is only mutable under l.mu (#1360, #1549).
		if ePtr.Type == "agent" || ePtr.Type == "task_start" {
			l.recordAgentRingPosLocked(ePtr.Type, ePtr.ToolUseID, ringIdx)
		}

		// Replay skips applyEntryStateLocked unconditionally (see
		// AppendBatchReplay); live entries skip it for types it ignores.
		if !isReplay && entryAffectsAgentState(ePtr.Type) {
			if fire, p := l.applyEntryStateLocked(*ePtr); fire {
				pendingDone = append(pendingDone, p)
			}
		}

		// Track last-of-kind summaries for one Store below (still under l.mu).
		// The "saw" flag is separate from the value so an empty final Summary
		// still overwrites, matching Append's unconditional store.
		if ePtr.Type == "user" {
			lastPrompt = ePtr.Summary
			sawPrompt = true
			userDelta++
		} else if ePtr.Type == "text" {
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
		storeAtomicString(&l.lastResponseSummary, textutil.StripMarkdown(lastResponse))
	}
	if sawActivity {
		storeAtomicString(&l.lastActivitySummary, lastActivity)
	}
	if userDelta > 0 {
		// Single add under l.mu so a concurrent Snapshot sees the batch's
		// cumulative count together with the other per-turn state.
		l.userTurnCount.Add(userDelta)
	}
	l.mu.Unlock()

	l.fireTaskDoneCallbacks(pendingDone)

	// Persist outside l.mu. sinkCopy holds the post-stamp, post-sanitize
	// entries in ring commit order (the Persister's write-order guarantee).
	// Replay batches never feed the sink even if sinkReady already flipped
	// (#1482). len==1 goes through the single-entry sink when wired.
	if !isReplay {
		if sinkOneSet {
			if !l.invokePersistSinkOne(sinkOne) {
				l.invokePersistSink([]clievent.EventEntry{sinkOne})
			}
		} else {
			l.invokePersistSink(sinkCopy)
		}
	}

	l.notifySubscribers()
}
