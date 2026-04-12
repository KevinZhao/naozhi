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

const maxPersistedHistory = 500

// processIface abstracts the CLI process lifecycle methods used by the router
// and session layer. *cli.Process satisfies this interface.
type processIface interface {
	Alive() bool
	IsRunning() bool
	Close()
	Interrupt()
	Send(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error)
	// Dashboard introspection
	GetSessionID() string
	GetState() cli.ProcessState
	TotalCost() float64
	EventEntries() []cli.EventEntry
	EventLastN(n int) []cli.EventEntry
	EventEntriesSince(afterMS int64) []cli.EventEntry
	ProtocolName() string
	SubscribeEvents() (<-chan struct{}, func())
	PID() int
	InjectHistory(entries []cli.EventEntry)
	TurnAgents() []cli.SubagentInfo
}

// processBox wraps processIface for use with atomic.Pointer (which requires a concrete type).
type processBox struct{ p processIface }

// ManagedSession wraps a claude CLI process with session metadata.
type ManagedSession struct {
	Key string

	// sessionID stores the CLI session ID atomically.
	// Written once during first successful Send, read by Snapshot lock-free.
	sessionID atomic.Value // stores string

	// onSessionID is called when a session ID is first captured from Send().
	// Set by the Router to track known IDs for history exclusion.
	onSessionID func(string)

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

	process     atomic.Pointer[processBox] // stores *processBox; use loadProcess/storeProcess
	sendMu      sync.Mutex                 // serializes messages to the same session
	sendCancel  atomic.Pointer[context.CancelFunc]
	workspace   string       // effective cwd at spawn time
	cliName     string       // "claude-code", "kiro" — set at creation from Wrapper
	cliVersion  string       // semver from --version — set at creation from Wrapper
	deathReason atomic.Value // string: why process died, empty if alive
	totalCost   float64      // cached cost when process is nil

	// persistedHistory stores event entries that survive process restarts.
	// Populated by InjectHistory and carried over when the process is replaced.
	persistedHistory []cli.EventEntry

	// prevSessionIDs tracks all previous session IDs for this key (oldest → newest).
	// Used on startup to load the full conversation chain from JSONL files.
	prevSessionIDs []string

	// Exempt marks this session as exempt from TTL cleanup, eviction, and activeCount.
	// Used for planner sessions that should persist indefinitely.
	Exempt bool
}

func (s *ManagedSession) loadProcess() processIface {
	if box := s.process.Load(); box != nil {
		return box.p
	}
	return nil
}

func (s *ManagedSession) storeProcess(p processIface) {
	if p == nil {
		s.process.Store(nil)
	} else {
		s.process.Store(&processBox{p: p})
	}
}

func (s *ManagedSession) isAlive() bool {
	p := s.loadProcess()
	return p != nil && p.Alive()
}

// ReattachProcess safely injects a reconnected shim process into this session.
// Called by Router.reconnectShims after naozhi restart.
func (s *ManagedSession) ReattachProcess(proc processIface, sessionID string) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	s.storeProcess(proc)
	s.setSessionID(sessionID)
	s.deathReason.Store("")
	s.lastActive.Store(time.Now().UnixNano())

	if s.onSessionID != nil && sessionID != "" {
		s.onSessionID(sessionID)
	}
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

	result, err := s.loadProcess().Send(ctx, text, images, wrappedOnEvent)
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
		if s.onSessionID != nil {
			s.onSessionID(result.SessionID)
		}
	}
	return result, nil
}

// Interrupt sends SIGINT to the CLI process and cancels the current Send context.
// This is the equivalent of pressing Escape in Claude Code.
//
// Context cancellation happens BEFORE acquiring sendMu so that an in-flight
// Send() can return via <-ctx.Done() and release the lock. Without this,
// Interrupt() would deadlock when Send() holds sendMu waiting for events.
func (s *ManagedSession) Interrupt() bool {
	// Cancel the in-flight Send context first (lock-free). This unblocks
	// Send() which holds sendMu, allowing us to proceed below.
	if cancel := s.sendCancel.Load(); cancel != nil {
		(*cancel)()
	}

	// loadProcess is atomic — no lock needed.
	proc := s.loadProcess()
	if proc == nil || !proc.Alive() {
		return false
	}

	proc.Interrupt()
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

// maxKeyComponent is the maximum length of a single session key component.
const maxKeyComponent = 128

// sanitizeKeyComponent truncates and strips colons from a session key component
// to prevent key confusion and unbounded map key growth.
func sanitizeKeyComponent(s string) string {
	s = strings.ReplaceAll(s, ":", "_")
	if len(s) > maxKeyComponent {
		s = s[:maxKeyComponent]
	}
	return s
}

// SessionKey builds a session key from components.
func SessionKey(platform, chatType, id, agentID string) string {
	if agentID == "" {
		agentID = "general"
	}
	return sanitizeKeyComponent(platform) + ":" + sanitizeKeyComponent(chatType) + ":" + sanitizeKeyComponent(id) + ":" + sanitizeKeyComponent(agentID)
}

// SessionSnapshot is a point-in-time view of a session for the dashboard API.
type SessionSnapshot struct {
	Key          string             `json:"key"`
	Platform     string             `json:"platform"`
	Agent        string             `json:"agent"`
	SessionID    string             `json:"session_id"`
	State        string             `json:"state"`
	Protocol     string             `json:"protocol"`
	CLIName      string             `json:"cli_name,omitempty"`    // "claude-code", "kiro"
	CLIVersion   string             `json:"cli_version,omitempty"` // e.g. "2.1.92"
	LastActive   int64              `json:"last_active"`           // unix ms
	TotalCost    float64            `json:"total_cost"`
	Workspace    string             `json:"workspace,omitempty"`
	DeathReason  string             `json:"death_reason,omitempty"`
	ChatType     string             `json:"chat_type,omitempty"`
	ChatID       string             `json:"chat_id,omitempty"`
	Node         string             `json:"node,omitempty"`
	LastPrompt   string             `json:"last_prompt,omitempty"`   // most recent user message
	LastActivity string             `json:"last_activity,omitempty"` // most recent tool/thinking status
	Summary      string             `json:"summary,omitempty"`       // Claude-generated session title
	Project      string             `json:"project,omitempty"`       // project name (filled by server)
	IsPlanner    bool               `json:"is_planner,omitempty"`    // true for project planner sessions
	Subagents    []cli.SubagentInfo `json:"subagents,omitempty"`     // active sub-agent types in current turn
}

func (s *ManagedSession) HasProcess() bool {
	return s.loadProcess() != nil
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
		CLIName:    s.cliName,
		CLIVersion: s.cliVersion,
	}
	if dr, ok := s.deathReason.Load().(string); ok {
		snap.DeathReason = dr
	}

	proc := s.loadProcess()
	if proc == nil {
		snap.TotalCost = s.totalCost
		snap.State = "suspended"
	} else {
		snap.State = proc.GetState().String()
		snap.Protocol = proc.ProtocolName()
		snap.TotalCost = proc.TotalCost()
		snap.Subagents = proc.TurnAgents()
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
	proc := s.loadProcess()
	if proc != nil {
		return proc.EventEntries()
	}
	s.sendMu.Lock()
	out := make([]cli.EventEntry, len(s.persistedHistory))
	copy(out, s.persistedHistory)
	s.sendMu.Unlock()
	return out
}

// EventLastN returns the most recent n event entries.
func (s *ManagedSession) EventLastN(n int) []cli.EventEntry {
	proc := s.loadProcess()
	if proc != nil {
		return proc.EventLastN(n)
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if n <= 0 || n >= len(s.persistedHistory) {
		out := make([]cli.EventEntry, len(s.persistedHistory))
		copy(out, s.persistedHistory)
		return out
	}
	start := len(s.persistedHistory) - n
	out := make([]cli.EventEntry, n)
	copy(out, s.persistedHistory[start:])
	return out
}

// EventEntriesSince returns the event log entries after the given unix ms timestamp.
func (s *ManagedSession) EventEntriesSince(afterMS int64) []cli.EventEntry {
	proc := s.loadProcess()
	if proc != nil {
		return proc.EventEntriesSince(afterMS)
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
func (s *ManagedSession) SubscribeEvents() (<-chan struct{}, func()) {
	proc := s.loadProcess()
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
	if len(s.persistedHistory) > maxPersistedHistory {
		trimmed := make([]cli.EventEntry, maxPersistedHistory)
		copy(trimmed, s.persistedHistory[len(s.persistedHistory)-maxPersistedHistory:])
		s.persistedHistory = trimmed
	}
	if p := s.loadProcess(); p != nil {
		p.InjectHistory(entries)
	}
	// Update cached snapshot values from injected history (only if not yet set by Send).
	// Scan from the end to find the last user/tool_use entries efficiently.
	var prompt, activity string
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if prompt == "" && e.Type == "user" {
			prompt = e.Summary
		}
		if activity == "" && (e.Type == "tool_use" || e.Type == "thinking") {
			activity = e.Summary
		}
		if prompt != "" && activity != "" {
			break
		}
	}
	if prompt != "" && s.lastPrompt.Load() == nil {
		s.lastPrompt.Store(prompt)
	}
	if activity != "" && s.lastActivity.Load() == nil {
		s.lastActivity.Store(activity)
	}
}
