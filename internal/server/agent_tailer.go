package server

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
)

// Agent tailer layer — streams each team agent's on-disk transcript to the
// dashboard via WebSocket. Lives here because it fans out to wsClient
// connections; parsing (cli.TranscriptReader) stays backend-agnostic.
//
// Lifecycle: ensureTailer (silent, buffers events) → attach (replay +
// live push) → closeTask (agent_done). detach to refCount==0 keeps the
// tailer silent until task_done or idle grace.

const (
	// agentTailerPollInterval is the file-stat/Tail cadence (50 tailers → 250 stats/s).
	agentTailerPollInterval = 200 * time.Millisecond

	// agentTailerIdleGrace drops a silent tailer (refCount==0) whose file has
	// not grown for this long (covers agents that finish without task_done).
	agentTailerIdleGrace = 30 * time.Second

	// agentTailerMax caps concurrent tailers per Hub; beyond it subscribers get
	// agent_subscribe_rejected{reason:"capacity"} and fall back to HTTP poll.
	agentTailerMax = 50
)

// agentTailer streams a single agent jsonl to any number of subscribed
// wsClients and tracks aggregate stats (LastTool/ToolUses/DurationMS) for
// enrichSnapshot consumers even when no client is listening.
type agentTailer struct {
	key       string
	taskID    string
	toolUseID string
	reader    *cli.TranscriptReader
	reg       *tailerRegistry
	hub       *Hub

	stopCh   chan struct{}
	doneOnce sync.Once

	mu         sync.Mutex
	subs       map[*wsClient]struct{}
	refCount   atomic.Int32 // mirrors len(subs)
	buffered   []clievent.EventEntry
	meta       node.AgentMetaPatch
	lastActive time.Time
	startedAt  time.Time
	closed     bool
}

// MetaSnapshot returns a copy of the tailer's meta without mutating state.
// Consumed by enrichSnapshot.
func (t *agentTailer) MetaSnapshot() node.AgentMetaPatch {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.meta
}

// pollOnce reads the next slice of transcript events. Returns false when the
// tailer has self-terminated (idle grace expired) or has already been closed.
// Invoked by the registry's central pollLoop; the t.closed check covers
// finalize() running between the snapshot and this call.
func (t *agentTailer) pollOnce() bool {
	events, err := t.reader.Tail()
	if err != nil {
		slog.Debug("agent_tailer: tail error", "key", t.key, "task", t.taskID, "err", err)
	}

	// Wall clock captured outside the lock to keep the critical section short (#1407).
	now := time.Now()

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return false
	}
	if len(events) > 0 {
		t.lastActive = now
		// Buffer for late subscribers, bounded at 500; oldest dropped first.
		for _, e := range events {
			t.buffered = append(t.buffered, e)
			t.updateMetaFromEventLocked(e, now)
		}
		if over := len(t.buffered) - 500; over > 0 {
			// In-place copy reuses the backing array (zero alloc in steady
			// state); the vacated suffix is zeroed before truncation so dropped
			// EventEntry pointers become GC-eligible immediately.
			n := copy(t.buffered, t.buffered[over:])
			for i := n; i < len(t.buffered); i++ {
				t.buffered[i] = clievent.EventEntry{}
			}
			t.buffered = t.buffered[:n]
		}
	}
	t.mu.Unlock()

	// Second short lock window: subs/meta snapshot + idle/refCount read.
	// idle and refCount MUST be read in the same critical section as the
	// subs snapshot, or an attach between the two locks could make us close a
	// tailer that just gained a subscriber. subs is only snapshotted (from a
	// sync.Pool, #865) when there are events and someone is subscribed.
	var subs []*wsClient
	var subsHandle tailerSubsHandle
	var meta node.AgentMetaPatch
	t.mu.Lock()
	if len(events) > 0 && len(t.subs) > 0 {
		subs, subsHandle = acquireTailerSubsSlice(len(t.subs))
		for c := range t.subs {
			subs = append(subs, c)
		}
		meta = t.meta
	}
	idle := now.Sub(t.lastActive) > agentTailerIdleGrace
	refCount := t.refCount.Load()
	t.mu.Unlock()
	defer releaseTailerSubsSlice(subs, subsHandle)

	// Broadcast: with >1 subscriber marshal each frame once and SendRaw;
	// the single-subscriber path keeps the SendJSON shortcut.
	for i := range events {
		e := events[i]
		if len(subs) == 1 {
			subs[0].SendJSON(node.ServerMsg{
				Type:   "agent_event",
				Key:    t.key,
				TaskID: t.taskID,
				Event:  &e,
			})
			continue
		}
		data, err := marshalPooled(node.ServerMsg{
			Type:   "agent_event",
			Key:    t.key,
			TaskID: t.taskID,
			Event:  &e,
		})
		if err != nil {
			// Marshal cannot fail in practice; fall back to per-subscriber
			// SendJSON rather than silently dropping the frame.
			for _, c := range subs {
				c.SendJSON(node.ServerMsg{
					Type:   "agent_event",
					Key:    t.key,
					TaskID: t.taskID,
					Event:  &e,
				})
			}
			continue
		}
		for _, c := range subs {
			c.SendRaw(data)
		}
	}
	if len(events) > 0 && len(subs) > 0 {
		m := meta
		if len(subs) == 1 {
			subs[0].SendJSON(node.ServerMsg{
				Type:      "agent_meta",
				Key:       t.key,
				TaskID:    t.taskID,
				AgentMeta: &m,
			})
		} else {
			data, err := marshalPooled(node.ServerMsg{
				Type:      "agent_meta",
				Key:       t.key,
				TaskID:    t.taskID,
				AgentMeta: &m,
			})
			if err != nil {
				for _, c := range subs {
					c.SendJSON(node.ServerMsg{
						Type:      "agent_meta",
						Key:       t.key,
						TaskID:    t.taskID,
						AgentMeta: &m,
					})
				}
			} else {
				for _, c := range subs {
					c.SendRaw(data)
				}
			}
		}
	}

	// Idle reap — only when truly silent. idle/refCount/subs were read in one
	// critical section, so this is not a TOCTOU.
	if idle && refCount == 0 {
		t.reg.closeTask(t.key, t.taskID, "")
		return false
	}
	return true
}

// updateMetaFromEventLocked refreshes meta counters from a single event.
// `now` is shared by all events of one pollOnce so DurationMS is consistent.
// Caller must hold t.mu.
func (t *agentTailer) updateMetaFromEventLocked(e clievent.EventEntry, now time.Time) {
	switch e.Type {
	case "tool_use":
		t.meta.ToolUses++
		if e.Tool != "" {
			t.meta.LastTool = e.Tool
		}
		if e.Summary != "" {
			t.meta.LastDetail = e.Summary
		}
	case "tool_result":
		// ToolUses already counted on tool_use; only refresh LastDetail.
		if e.Summary != "" {
			t.meta.LastDetail = e.Summary
		}
	case "thinking":
		// Not a tool use, but advances the "doing right now" line.
		t.meta.LastTool = "thinking"
	}
	if !t.startedAt.IsZero() {
		t.meta.DurationMS = now.Sub(t.startedAt).Milliseconds()
	}
}

// finalize stops the tailer, fires agent_done to all subscribers, and nudges
// a final agent_meta so the banner row's final "N calls · 2.1s" stays
// accurate even after the user's view has unsubscribed.
func (t *agentTailer) finalize(status string) {
	t.doneOnce.Do(func() {
		close(t.stopCh)
		t.mu.Lock()
		t.closed = true
		subs := make([]*wsClient, 0, len(t.subs))
		for c := range t.subs {
			subs = append(subs, c)
		}
		meta := t.meta
		t.subs = nil
		t.refCount.Store(0)
		t.mu.Unlock()

		// Release the transcript fd eagerly; t.closed=true plus the byTask
		// removal stop the poller from re-Tailing, and Close() is idempotent (#1807).
		if t.reader != nil {
			_ = t.reader.Close()
		}

		if status == "" {
			status = "completed"
		}
		m := meta
		metaMsg := node.ServerMsg{
			Type:      "agent_meta",
			Key:       t.key,
			TaskID:    t.taskID,
			AgentMeta: &m,
		}
		doneMsg := node.ServerMsg{
			Type:   "agent_done",
			Key:    t.key,
			TaskID: t.taskID,
			Status: status,
		}
		// Mirror pollOnce: marshal once + SendRaw for multi-tab fan-out;
		// single subscriber keeps SendJSON; marshal error falls back per-sub.
		if len(subs) == 1 {
			subs[0].SendJSON(metaMsg)
			subs[0].SendJSON(doneMsg)
			return
		}
		metaData, metaErr := marshalPooled(metaMsg)
		doneData, doneErr := marshalPooled(doneMsg)
		for _, c := range subs {
			if metaErr != nil {
				c.SendJSON(metaMsg)
			} else {
				c.SendRaw(metaData)
			}
			if doneErr != nil {
				c.SendJSON(doneMsg)
			} else {
				c.SendRaw(doneData)
			}
		}
	})
}
