package cli

import (
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// maxInflightTools bounds the tracker: a turn fanning out more parallel tools
// than this still reports one of them, and a stream that never closes its
// tool_use blocks cannot grow the slice.
const maxInflightTools = 32

// inflightTools records the tools the backend has started and not yet
// finished, so a no-output watchdog kill can tell a silent tool from a silent
// model. readLoop writes it from every frame; the watchdog reads it.
type inflightTools struct {
	mu    sync.Mutex
	tools []inflightTool
}

type inflightTool struct {
	id      string
	name    string
	started time.Time
}

// observe updates the tracker from one backend frame. Claude opens a tool with
// an assistant tool_use block and closes it with a user tool_result block;
// ACP and codex send assistant tool_use / tool_result subtypes keyed by
// ToolUseID. A result ends the turn and with it every tool.
func (t *inflightTools) observe(ev clievent.Event, now time.Time) {
	switch ev.Type {
	case "result":
		t.mu.Lock()
		t.tools = t.tools[:0]
		t.mu.Unlock()
	case "tool_progress":
		id := ev.ParentToolUseID
		if id == "" {
			id = ev.ToolUseID
		}
		t.start(id, ev.ToolName, now)
	case "user":
		if ev.Message == nil {
			return
		}
		for _, b := range ev.Message.Content {
			if b.Type == "tool_result" {
				t.finish(b.ToolUseID)
			}
		}
	case "assistant":
		if ev.SubType == "tool_result" {
			if ev.ToolCall == nil {
				return
			}
			if toolCallRunning(ev.ToolCall.Status) {
				t.start(ev.ToolUseID, ev.ToolCall.Title, now)
			} else {
				t.finish(ev.ToolUseID)
			}
			return
		}
		if ev.Message == nil {
			return
		}
		for _, b := range ev.Message.Content {
			if b.Type != "tool_use" {
				continue
			}
			id := b.ID
			if id == "" {
				id = ev.ToolUseID
			}
			t.start(id, b.Name, now)
		}
	}
}

// toolCallRunning reports whether an ACP / codex tool status is non-terminal.
// Anything else (completed, failed, declined, cancelled) closes the tool.
func toolCallRunning(status string) bool {
	switch status {
	case "", "pending", "in_progress", "inProgress":
		return true
	}
	return false
}

// start records a tool unless it is already tracked; a heartbeat for a known
// tool keeps the original start time. An id-less tool is skipped because
// nothing could ever close it.
func (t *inflightTools) start(id, name string, now time.Time) {
	if id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.tools {
		if t.tools[i].id == id {
			if t.tools[i].name == "" {
				t.tools[i].name = name
			}
			return
		}
	}
	if len(t.tools) < maxInflightTools {
		t.tools = append(t.tools, inflightTool{id: id, name: name, started: now})
	}
}

func (t *inflightTools) finish(id string) {
	if id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.tools {
		if t.tools[i].id == id {
			t.tools = append(t.tools[:i], t.tools[i+1:]...)
			return
		}
	}
}

// oldest returns the longest-running tracked tool; ok is false when no tool is
// in flight. Entries are appended in start order, so that is the first one.
func (t *inflightTools) oldest() (tool inflightTool, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.tools) == 0 {
		return inflightTool{}, false
	}
	return t.tools[0], true
}
