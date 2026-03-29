package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// processIface abstracts the CLI process lifecycle methods used by the router
// and session layer. *cli.Process satisfies this interface.
type processIface interface {
	Alive() bool
	IsRunning() bool
	Close()
	Interrupt()
	Send(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error)
	// Dashboard introspection
	GetState() cli.ProcessState
	TotalCost() float64
	EventEntries() []cli.EventEntry
	EventEntriesSince(afterMS int64) []cli.EventEntry
	ProtocolName() string
	SubscribeEvents() (<-chan struct{}, func())
	PID() int
	InjectHistory(entries []cli.EventEntry)
}

// ManagedSession wraps a claude CLI process with session metadata.
type ManagedSession struct {
	Key string

	// sessionID stores the CLI session ID atomically.
	// Written once during first successful Send, read by Snapshot lock-free.
	sessionID atomic.Value // stores string

	// lastActive stores time.UnixNano atomically to avoid data races
	// between Send() (under sendMu) and Cleanup/evictOldest (under r.mu).
	lastActive atomic.Int64

	// lastPrompt caches the most recent user message summary (atomic for lock-free Snapshot reads).
	lastPrompt atomic.Value // stores string

	// lastActivity caches the most recent tool_use/thinking summary.
	lastActivity atomic.Value // stores string

	// Cached key parts, parsed once via keyOnce. Key is immutable.
	keyOnce     sync.Once
	keyPlatform string
	keyChatType string
	keyChatID   string
	keyAgentID  string

	process     processIface
	sendMu      sync.Mutex // serializes messages to the same session
	sendCancel  atomic.Pointer[context.CancelFunc]
	workspace   string       // effective cwd at spawn time
	deathReason atomic.Value // string: why process died, empty if alive
	totalCost   float64      // cached cost when process is nil

	// persistedHistory stores event entries that survive process restarts.
	// Populated by InjectHistory and carried over when the process is replaced.
	persistedHistory []cli.EventEntry
}

// GetLastActive returns the last active time.
func (s *ManagedSession) GetLastActive() time.Time {
	return time.Unix(0, s.lastActive.Load())
}

// touchLastActive updates the last active timestamp.
func (s *ManagedSession) touchLastActive() {
	s.lastActive.Store(time.Now().UnixNano())
}

// Send delivers a message to the claude process and returns the result.
// Messages to the same session are serialized via sendMu.
func (s *ManagedSession) Send(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	s.sendCancel.Store(&cancel)
	defer func() {
		s.sendCancel.Store(nil)
		cancel()
	}()

	s.touchLastActive()

	// Cache the user prompt for Snapshot (matches how process.go logs user events).
	prompt := cli.TruncateRunes(text, 120)
	if len(images) > 0 {
		prompt += fmt.Sprintf(" [+%d image(s)]", len(images))
	}
	s.lastPrompt.Store(prompt)

	// Wrap onEvent to track last tool_use/thinking for Snapshot.
	wrappedOnEvent := func(ev cli.Event) {
		if ev.Type == "assistant" && ev.Message != nil {
			for _, block := range ev.Message.Content {
				if block.Type == "thinking" {
					s.lastActivity.Store(cli.TruncateRunes(block.Text, 120))
					break
				}
				if block.Type == "tool_use" {
					s.lastActivity.Store(block.Name)
					break
				}
			}
		}
		if onEvent != nil {
			onEvent(ev)
		}
	}

	result, err := s.process.Send(ctx, text, images, wrappedOnEvent)
	if err != nil {
		if errors.Is(err, cli.ErrNoOutputTimeout) {
			s.deathReason.Store("no_output_timeout")
		} else if errors.Is(err, cli.ErrTotalTimeout) {
			s.deathReason.Store("total_timeout")
		}
		return nil, err
	}

	// Capture session ID from first successful send
	if s.getSessionID() == "" && result.SessionID != "" {
		s.setSessionID(result.SessionID)
	}
	return result, nil
}

// Interrupt sends SIGINT to the CLI process and cancels the current Send context.
// This is the equivalent of pressing Escape in Claude Code.
func (s *ManagedSession) Interrupt() bool {
	s.sendMu.Lock()
	proc := s.process
	s.sendMu.Unlock()

	if proc == nil || !proc.IsRunning() {
		return false
	}

	proc.Interrupt()
	if cp := s.sendCancel.Load(); cp != nil {
		(*cp)()
	}
	return true
}

// getSessionID returns the session ID lock-free via atomic.Value.
func (s *ManagedSession) getSessionID() string {
	v := s.sessionID.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

// setSessionID stores the session ID atomically.
func (s *ManagedSession) setSessionID(id string) {
	s.sessionID.Store(id)
}

// parseKeyParts lazily parses the immutable session key into cached components.
func (s *ManagedSession) parseKeyParts() {
	s.keyOnce.Do(func() {
		parts := strings.SplitN(s.Key, ":", 4)
		if len(parts) >= 1 {
			s.keyPlatform = parts[0]
		}
		if len(parts) >= 2 {
			s.keyChatType = parts[1]
		}
		if len(parts) >= 3 {
			s.keyChatID = parts[2]
		}
		if len(parts) >= 4 {
			s.keyAgentID = parts[3]
		}
	})
}

// SessionKey builds a session key from components.
func SessionKey(platform, chatType, id, agentID string) string {
	if agentID == "" {
		agentID = "general"
	}
	return platform + ":" + chatType + ":" + id + ":" + agentID
}

// SessionSnapshot is a point-in-time view of a session for the dashboard API.
type SessionSnapshot struct {
	Key          string  `json:"key"`
	Platform     string  `json:"platform"`
	Agent        string  `json:"agent"`
	SessionID    string  `json:"session_id"`
	State        string  `json:"state"`
	Protocol     string  `json:"protocol"`
	LastActive   int64   `json:"last_active"` // unix ms
	TotalCost    float64 `json:"total_cost"`
	Workspace    string  `json:"workspace,omitempty"`
	DeathReason  string  `json:"death_reason,omitempty"`
	ChatType     string  `json:"chat_type,omitempty"`
	ChatID       string  `json:"chat_id,omitempty"`
	Node         string  `json:"node,omitempty"`
	LastPrompt   string  `json:"last_prompt,omitempty"`   // most recent user message
	LastActivity string  `json:"last_activity,omitempty"` // most recent tool/thinking status
}

// Snapshot returns a point-in-time view of this session.
func (s *ManagedSession) Snapshot() SessionSnapshot {
	s.parseKeyParts()
	snap := SessionSnapshot{
		Key:        s.Key,
		Platform:   s.keyPlatform,
		ChatType:   s.keyChatType,
		ChatID:     s.keyChatID,
		Agent:      s.keyAgentID,
		SessionID:  s.getSessionID(),
		LastActive: s.GetLastActive().UnixMilli(),
		Workspace:  s.workspace,
	}
	if dr, ok := s.deathReason.Load().(string); ok {
		snap.DeathReason = dr
	}

	if s.process == nil {
		snap.TotalCost = s.totalCost
		if snap.SessionID != "" {
			snap.State = "suspended"
		} else {
			snap.State = "dead"
		}
	} else {
		snap.State = s.process.GetState().String()
		snap.Protocol = s.process.ProtocolName()
		snap.TotalCost = s.process.TotalCost()
	}

	// Read cached values instead of copying the full event log.
	if v := s.lastPrompt.Load(); v != nil {
		snap.LastPrompt = v.(string)
	}
	if v := s.lastActivity.Load(); v != nil {
		snap.LastActivity = v.(string)
	}

	return snap
}

// EventEntries returns the event log entries for this session.
// Returns persisted history when the process is nil or dead.
func (s *ManagedSession) EventEntries() []cli.EventEntry {
	if s.process != nil {
		return s.process.EventEntries()
	}
	s.sendMu.Lock()
	out := make([]cli.EventEntry, len(s.persistedHistory))
	copy(out, s.persistedHistory)
	s.sendMu.Unlock()
	return out
}

// EventEntriesSince returns the event log entries after the given unix ms timestamp.
func (s *ManagedSession) EventEntriesSince(afterMS int64) []cli.EventEntry {
	if s.process != nil {
		return s.process.EventEntriesSince(afterMS)
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	for i, e := range s.persistedHistory {
		if e.Time > afterMS {
			out := make([]cli.EventEntry, len(s.persistedHistory)-i)
			copy(out, s.persistedHistory[i:])
			return out
		}
	}
	return nil
}

// SubscribeEvents subscribes to event log notifications for this session.
// If the session has no process, returns a closed channel and a no-op unsubscribe.
//
// NOTE: This intentionally does NOT acquire sendMu. The process field is set
// once during spawnSession (under the router lock) before any Send() call,
// so reading it here is safe. Acquiring sendMu would block until an in-flight
// Send() completes, preventing real-time event streaming to dashboard subscribers.
func (s *ManagedSession) SubscribeEvents() (<-chan struct{}, func()) {
	proc := s.process
	if proc == nil {
		ch := make(chan struct{})
		close(ch)
		return ch, func() {}
	}
	return proc.SubscribeEvents()
}

// InjectHistory pre-populates the event log with historical entries.
// Entries are saved to persistedHistory so they survive process restarts.
func (s *ManagedSession) InjectHistory(entries []cli.EventEntry) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.persistedHistory = append(s.persistedHistory, entries...)
	if s.process != nil {
		s.process.InjectHistory(entries)
	}
	// Update cached snapshot values from injected history (only if not yet set by Send).
	var prompt, activity string
	for _, e := range entries {
		switch e.Type {
		case "user":
			prompt = e.Summary
		case "tool_use", "thinking":
			activity = e.Summary
		}
	}
	if prompt != "" && s.lastPrompt.Load() == nil {
		s.lastPrompt.Store(prompt)
	}
	if activity != "" && s.lastActivity.Load() == nil {
		s.lastActivity.Store(activity)
	}
}
