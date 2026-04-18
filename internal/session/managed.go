package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cli"
)

const maxPersistedHistory = 500

// processIface abstracts the CLI process lifecycle methods used by the router
// and session layer. *cli.Process satisfies this interface.
type processIface interface {
	Alive() bool
	IsRunning() bool
	Close()
	Kill()
	Interrupt()
	Send(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error)
	// Dashboard introspection
	GetSessionID() string
	GetState() cli.ProcessState
	TotalCost() float64
	EventEntries() []cli.EventEntry
	EventLastN(n int) []cli.EventEntry
	EventEntriesSince(afterMS int64) []cli.EventEntry
	LastEntryOfType(typ string) cli.EventEntry
	LastActivitySummary() string
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
	key string

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
	historyMu   sync.RWMutex               // protects persistedHistory reads/writes (independent of sendMu)
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

	// exempt marks this session as exempt from TTL cleanup, eviction, and activeCount.
	// Used for planner sessions that should persist indefinitely.
	exempt bool
}

// SessionKey returns the immutable session key.
func (s *ManagedSession) SessionKey() string { return s.key }

// IsExempt returns whether this session is exempt from TTL and eviction.
func (s *ManagedSession) IsExempt() bool { return s.exempt }

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

// ReattachProcessNoCallback is like ReattachProcess but skips the onSessionID
// callback. Used when the caller already holds router.mu and will track the
// session ID directly (avoids deadlock since onSessionID acquires router.mu).
//
// Does NOT acquire sendMu: all operations here are atomic stores, and the
// caller already holds router.mu (write). Acquiring sendMu here would violate
// the documented lock ordering (sendMu → router.mu) and risk ABBA deadlock
// with Send() which holds sendMu then calls onSessionID → router.mu.
//
// SAFETY CONSTRAINT: this function must only be called when Send() cannot be
// in flight for this session (e.g., during ReconnectShims at startup, or while
// the session's process is known-dead). If Send() were concurrently executing,
// the deathReason.Store("") here could silently erase a diagnostic death reason
// that Send() just set. The lack of sendMu makes this a logical race on the
// deathReason value, even though each individual Store is atomic.
func (s *ManagedSession) ReattachProcessNoCallback(proc processIface, sessionID string) {
	s.storeProcess(proc)
	s.setSessionID(sessionID)
	s.deathReason.Store("")
	s.lastActive.Store(time.Now().UnixNano())
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

	proc := s.loadProcess()
	if proc == nil {
		return nil, fmt.Errorf("session %s has no active process", s.key)
	}

	// lastActivity tracking is handled lock-free by EventLog.Append via its
	// cached lastActivitySummary; Snapshot() reads that value when the process
	// is alive. Passing onEvent directly (no wrapper closure) avoids a per-Send
	// heap allocation on the nil-callback path (cron/connector) and one less
	// indirect call per event on the Send path.
	result, err := proc.Send(ctx, text, images, onEvent)
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
// proc.Interrupt() is called BEFORE cancel() to ensure the interrupted flag is
// set before a new Send() can start. proc.Interrupt() only acquires shimWMu
// (not sendMu), so there is no deadlock risk. The subsequent cancel() unblocks
// any in-flight Send() waiting on ctx.Done(), allowing it to release sendMu.
//
// If cancel() were called first, a new Send could race in before proc.Interrupt()
// sets the interrupted flag, causing drainStaleEvents to miss stale events from
// the interrupted turn — the old result would then be returned as the new turn's
// response.
func (s *ManagedSession) Interrupt() bool {
	proc := s.loadProcess()
	if proc == nil || !proc.Alive() {
		// Still cancel in case Send is blocked on ctx.Done().
		if cancel := s.sendCancel.Load(); cancel != nil {
			(*cancel)()
		}
		return false
	}

	proc.Interrupt()

	if cancel := s.sendCancel.Load(); cancel != nil {
		(*cancel)()
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
		parts := strings.SplitN(s.key, ":", 4)
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
//
// Fast path: most session-key components are short ASCII without colons
// (platform IDs, agent names, chat IDs). Avoid ReplaceAll+RuneCount allocations
// in that common case.
func sanitizeKeyComponent(s string) string {
	if len(s) <= maxKeyComponent {
		ok := true
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c == ':' || c >= 0x80 {
				ok = false
				break
			}
		}
		if ok {
			return s
		}
	}
	s = strings.ReplaceAll(s, ":", "_")
	if utf8.RuneCountInString(s) > maxKeyComponent {
		runes := []rune(s)
		s = string(runes[:maxKeyComponent])
	}
	return s
}

// SanitizeCWDKey converts a filesystem path to a safe session-key component
// by stripping the leading slash, replacing path separators and colons,
// and truncating to maxKeyComponent.
func SanitizeCWDKey(cwd string) string {
	s := strings.ReplaceAll(strings.TrimPrefix(cwd, "/"), "/", "-")
	return sanitizeKeyComponent(s)
}

// SessionKey builds a session key from components.
func SessionKey(platform, chatType, id, agentID string) string {
	if agentID == "" {
		agentID = "general"
	}
	return sanitizeKeyComponent(platform) + ":" + sanitizeKeyComponent(chatType) + ":" + sanitizeKeyComponent(id) + ":" + sanitizeKeyComponent(agentID)
}

// TakeoverKey builds a session key for a takeover from a discovered process CWD.
func TakeoverKey(cwdKey string) string {
	return "local:takeover:" + cwdKey + ":general"
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
		Key:        s.key,
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
		snap.State = "ready"
	} else {
		snap.State = proc.GetState().String()
		snap.Protocol = proc.ProtocolName()
		snap.TotalCost = proc.TotalCost()
		snap.Subagents = proc.TurnAgents()
		// Prefer the EventLog-maintained summary (updated lock-free on every
		// event) so we don't need a wrapper closure around Send just to track
		// lastActivity.
		snap.LastActivity = proc.LastActivitySummary()
	}

	// Read cached values instead of copying the full event log.
	if v := s.lastPrompt.Load(); v != nil {
		snap.LastPrompt = v.(string)
	}
	if snap.LastActivity == "" {
		if v := s.lastActivity.Load(); v != nil {
			snap.LastActivity = v.(string)
		}
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
	s.historyMu.RLock()
	out := make([]cli.EventEntry, len(s.persistedHistory))
	copy(out, s.persistedHistory)
	s.historyMu.RUnlock()
	return out
}

// EventLastN returns the most recent n event entries.
func (s *ManagedSession) EventLastN(n int) []cli.EventEntry {
	proc := s.loadProcess()
	if proc != nil {
		return proc.EventLastN(n)
	}
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
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
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	// Linear scan from the end to find the boundary. persistedHistory may not be
	// strictly sorted by Time (multiple InjectHistory calls can interleave sessions),
	// so binary search (sort.Search) would be incorrect.
	start := len(s.persistedHistory)
	for start > 0 && s.persistedHistory[start-1].Time > afterMS {
		start--
	}
	if start < len(s.persistedHistory) {
		out := make([]cli.EventEntry, len(s.persistedHistory)-start)
		copy(out, s.persistedHistory[start:])
		return out
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
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	if len(entries) >= maxPersistedHistory {
		entries = entries[len(entries)-maxPersistedHistory:]
	}
	s.persistedHistory = append(s.persistedHistory, entries...)
	if len(s.persistedHistory) > maxPersistedHistory {
		s.persistedHistory = s.persistedHistory[len(s.persistedHistory)-maxPersistedHistory:]
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

// extractLastPromptFromProcess scans the attached process's event log to populate
// lastPrompt and lastActivity when they haven't been set yet (e.g. after shim reconnect
// where events were injected directly into the process, bypassing InjectHistory).
func (s *ManagedSession) extractLastPromptFromProcess() {
	if s.lastPrompt.Load() != nil && s.lastActivity.Load() != nil {
		return
	}
	p := s.loadProcess()
	if p == nil {
		return
	}
	entries := p.EventEntries()
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
