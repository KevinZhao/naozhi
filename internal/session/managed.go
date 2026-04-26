package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cli"
)

const (
	maxPersistedHistory = 500

	// maxPrevSessionIDs caps the session chain length so long-lived chats
	// don't grow storeEntry.PrevSessionIDs without bound (each "/new" or
	// workspace switch appends one). 32 retains enough chain for multi-day
	// context recovery while keeping sessions.json size bounded.
	maxPrevSessionIDs = 32
)

// processIface abstracts the CLI process lifecycle methods used by the router
// and session layer. *cli.Process satisfies this interface.
type processIface interface {
	Alive() bool
	IsRunning() bool
	Close()
	Kill()
	Interrupt()
	// InterruptViaControl asks the CLI to abort the active turn via an
	// in-band stream-json control_request (no SIGINT, no process kill).
	// Returns cli.ErrInterruptUnsupported for protocols without this primitive.
	InterruptViaControl() error
	Send(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error)
	// Dashboard introspection
	GetSessionID() string
	GetState() cli.ProcessState
	// DeathReason returns the process-level reason string recorded when the
	// shim-backed CLI exited (passive death). Empty while alive or when the
	// reason has not been classified yet.
	DeathReason() string
	TotalCost() float64
	EventEntries() []cli.EventEntry
	EventLastN(n int) []cli.EventEntry
	EventEntriesSince(afterMS int64) []cli.EventEntry
	EventEntriesBefore(beforeMS int64, limit int) []cli.EventEntry
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

	process    atomic.Pointer[processBox] // stores *processBox; use loadProcess/storeProcess
	sendMu     sync.Mutex                 // serializes messages to the same session
	historyMu  sync.RWMutex               // protects persistedHistory reads/writes (independent of sendMu)
	sendCancel atomic.Pointer[context.CancelFunc]
	workspace  string // effective cwd at spawn time
	// backend/cliName/cliVersion are written at spawn time AND later by
	// reconnectShims under r.mu (write), but read by Snapshot() without
	// any lock (called via ListSessions which only holds RLock while
	// collecting refs). Using atomic.Value keeps the read/write race-free
	// without round-tripping Snapshot through r.mu. Stored type is string.
	backend     atomic.Value // string: backend ID ("claude" | "kiro"); empty = router default
	cliName     atomic.Value // string: "claude-code", "kiro" — set at creation from Wrapper
	cliVersion  atomic.Value // string: semver from --version
	deathReason atomic.Value // string: why process died, empty if alive
	// userLabel is an operator-set display name that overrides summary/last_prompt
	// in the dashboard sidebar and header. Empty = unset, fall back to
	// summary → last_prompt. Lock-free reads from Snapshot() mirror the
	// backend/cliName/cliVersion pattern. Stored type is string.
	userLabel atomic.Value
	// totalCost is the cumulative cost carried over from a previous process
	// incarnation: written once at construction (either in NewRouter() when
	// restoring from store, or in spawnSession() when inheriting from the
	// replaced session) and read-only thereafter. Snapshot() falls back to
	// this value when the live process hasn't yet reported a result event —
	// this avoids the $0.00 flash after resume/reconnect. Per-instance
	// immutability means no sync is needed.
	totalCost float64

	// persistedHistory stores event entries that survive process restarts.
	// Populated by InjectHistory and carried over when the process is replaced.
	persistedHistory []cli.EventEntry

	// prevSessionIDs tracks previous session IDs for this key (oldest → newest).
	// Used on startup to load the full conversation chain from JSONL files.
	// Capped at maxPrevSessionIDs to bound long-lived session memory and
	// sessions.json size. Overflow drops oldest entries; history still loads
	// from the retained tail which carries the most recent context.
	prevSessionIDs []string

	// exempt marks this session as exempt from TTL cleanup, eviction, and activeCount.
	// Used for planner sessions that should persist indefinitely.
	exempt bool
}

// SessionKey returns the immutable session key.
func (s *ManagedSession) SessionKey() string { return s.key }

// IsExempt returns whether this session is exempt from TTL and eviction.
func (s *ManagedSession) IsExempt() bool { return s.exempt }

// loadStringAtomic reads a *atomic.Value known to store a string, returning
// "" when the value has never been written.
func loadStringAtomic(v *atomic.Value) string {
	if raw := v.Load(); raw != nil {
		if s, ok := raw.(string); ok {
			return s
		}
	}
	return ""
}

// Backend returns the backend ID ("" when the router default is in effect).
func (s *ManagedSession) Backend() string { return loadStringAtomic(&s.backend) }

// SetBackend records the backend ID for this session. Called at spawn time
// and (rarely) by reconnectShims after a naozhi restart.
func (s *ManagedSession) SetBackend(id string) { s.backend.Store(id) }

// CLIName returns the CLI display name (e.g. "claude-code", "kiro").
func (s *ManagedSession) CLIName() string { return loadStringAtomic(&s.cliName) }

// SetCLIName records the wrapper-provided CLI display name.
func (s *ManagedSession) SetCLIName(name string) { s.cliName.Store(name) }

// CLIVersion returns the detected CLI version string.
func (s *ManagedSession) CLIVersion() string { return loadStringAtomic(&s.cliVersion) }

// SetCLIVersion records the wrapper-provided CLI version.
func (s *ManagedSession) SetCLIVersion(v string) { s.cliVersion.Store(v) }

// UserLabel returns the operator-set display label ("" when unset).
func (s *ManagedSession) UserLabel() string { return loadStringAtomic(&s.userLabel) }

// SetUserLabel records an operator-set display label. Callers must have
// already validated length/charset; the empty string clears any prior label.
func (s *ManagedSession) SetUserLabel(v string) { s.userLabel.Store(v) }

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
		switch {
		case errors.Is(err, cli.ErrNoOutputTimeout):
			s.deathReason.Store("no_output_timeout")
		case errors.Is(err, cli.ErrTotalTimeout):
			s.deathReason.Store("total_timeout")
		case errors.Is(err, cli.ErrProcessExited):
			// Prefer the precise reason recorded by readLoop (e.g.
			// cli_exited_code_1, shim_eof, readloop_panic) over a generic
			// "process_exited" so operators can tell a crash from a clean exit.
			reason := "process_exited"
			if dr := proc.DeathReason(); dr != "" {
				reason = dr
			}
			s.deathReason.Store(reason)
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

// InterruptOutcome describes what happened on an InterruptViaControl call.
// Callers use this instead of a bare bool so log messages can reflect the
// actual state (e.g. don't claim "aborted turn" when nothing was running).
type InterruptOutcome int

const (
	// InterruptSent — a control_request reached the CLI; the active turn
	// will produce a final result shortly and the next Send() will drain it.
	InterruptSent InterruptOutcome = iota
	// InterruptNoSession — session does not exist or has no live process.
	InterruptNoSession
	// InterruptNoTurn — session is alive but idle; nothing was interrupted.
	InterruptNoTurn
	// InterruptUnsupported — protocol does not support stdin-level interrupt
	// (e.g. ACP). Callers may fall back to Interrupt() for SIGINT semantics.
	InterruptUnsupported
	// InterruptError — transport failure (shim socket dead, write broke);
	// the process-level settle flags have been rolled back. Callers should
	// log this as an error.
	InterruptError
)

// InterruptViaControl asks the CLI to abort the active turn by writing an
// in-band control_request to stdin. Unlike Interrupt, this does NOT cancel
// the Send() context — the in-flight Send will see the CLI's interrupted
// result event arrive naturally and return normally, so the owner loop can
// proceed to drain and send the coalesced follow-up messages on the same
// live process.
//
// Transport failures are logged at Warn here (rather than silently returned)
// so operators do not need every caller to plumb their own error log; the
// outcome return value still lets callers tune their user-facing text.
func (s *ManagedSession) InterruptViaControl() InterruptOutcome {
	proc := s.loadProcess()
	if proc == nil || !proc.Alive() {
		return InterruptNoSession
	}
	err := proc.InterruptViaControl()
	if err == nil {
		return InterruptSent
	}
	switch {
	case errors.Is(err, cli.ErrNoActiveTurn):
		return InterruptNoTurn
	case errors.Is(err, cli.ErrInterruptUnsupported):
		// Caller decides whether to fall back; do not escalate to SIGINT
		// silently because that would couple two different semantics.
		return InterruptUnsupported
	default:
		// Transport / write error. Process.InterruptViaControl has already
		// rolled back the settle flags, so the next Send() will not spin
		// on the 500ms settle timeout. Surface at Warn so the failure mode
		// is visible even to callers that treat non-Sent as "fall back".
		slog.Warn("session interrupt via control_request failed",
			"session_key", s.key, "err", err)
		return InterruptError
	}
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
			// Reject colons (reserved key separator), 8-bit bytes (non-ASCII
			// IDs are truncated to maxKeyComponent via the rune path below),
			// and ALL C0 control bytes including tab, plus DEL (0x7f). Control
			// bytes can travel through IM-originated chat IDs into
			// slog.TextHandler attrs and fragment log lines: \n injects fake
			// entries, \x1b rewrites terminal output via ANSI, and \t is the
			// key=value separator for slog.TextHandler — a tab in a chat ID
			// would split one attr into two. The slow path (strings.Map
			// below) mirrors this gate byte-for-byte so the two paths agree.
			// R60-GO-M1 / R61-GO-6.
			if c == ':' || c >= 0x80 || c < 0x20 || c == 0x7f {
				ok = false
				break
			}
		}
		if ok {
			return s
		}
	}
	s = strings.ReplaceAll(s, ":", "_")
	// Drop ALL C0 control bytes (including tab) AND Unicode formatting/bidi chars
	// that terminal log viewers render as invisible or swap-displayed:
	//   - U+2028/U+2029 LINE/PARAGRAPH SEPARATOR are treated as newlines by
	//     some JSON log consumers → log-line injection.
	//   - U+202A..U+202E (embedding/override/pop) flip terminal output
	//     left-to-right, letting an attacker mask fabricated log content
	//     under `tail -f` / `journalctl`.
	//   - U+200B..U+200F (zero-width space / joiner / LTR/RTL mark) are
	//     invisible; unsafe for human-readable log attrs.
	//   - U+FEFF BOM is invisible.
	// These classes aren't covered by the C0 gate in the fast path and would
	// otherwise slip through for chat IDs whose byte length fits in one
	// Unicode codepoint (3 bytes for 2028/2029, also mapped per-rune here).
	// Done via strings.Map because the ReplaceAll-based fast path is 1:1
	// on bytes; rune-truncation below handles any multi-byte tail.
	s = strings.Map(func(r rune) rune {
		// Strip ALL C0 controls including tab; slog.TextHandler uses tab as
		// the key/value separator so an embedded tab would fragment one attr
		// into two. Matches the fast-path gate above. R60-GO-M1.
		//
		// Also strip DEL (U+007F) and the C1 control range (U+0080..U+009F).
		// The fast-path byte gate rejects 8-bit *bytes*, but a chat ID that
		// arrives as valid UTF-8 containing a C1 codepoint (encoded as
		// 0xC2 0x80..0xC2 0x9F) takes the slow path because the first byte
		// (0xC2) is ≥ 0x80. Without this branch the C1 codepoint survives
		// and terminals may interpret it as a control function. R61-GO-6.
		if r < 0x20 || (r >= 0x7F && r <= 0x9F) {
			return '_'
		}
		switch {
		case r >= 0x200B && r <= 0x200F, // zero-width space / joiner / LTR/RTL mark
			r >= 0x202A && r <= 0x202E, // embedding / override / pop
			r == 0x2028, r == 0x2029,   // line/paragraph separator
			r == 0xFEFF: // BOM
			return '_'
		}
		return r
	}, s)
	// Cheap byte-length gate first: UTF-8 byte length is always ≥ rune count,
	// so strings with ≤ maxKeyComponent bytes cannot exceed maxKeyComponent
	// runes. Only pay for RuneCountInString + []rune conversion when byte
	// length actually exceeds the cap. The common case (sanitize reached
	// only because of a colon or embedded control byte) skips both allocs.
	// R64-PERF-8.
	if len(s) > maxKeyComponent && utf8.RuneCountInString(s) > maxKeyComponent {
		runes := []rune(s)
		s = string(runes[:maxKeyComponent])
	}
	return s
}

// SanitizeLogAttr returns a version of s that is safe to feed directly into
// slog attributes without fragmenting log lines. Uses the same rules as
// session-key components: strips colons, 8-bit bytes, C0 control bytes, and
// Unicode bidi/zero-width chars; truncates to maxKeyComponent runes. Call
// this on any IM-originated string (chat ID, user ID, raw incoming key)
// BEFORE passing it to slog.With / slog.*Context so an attacker-controlled
// chat ID cannot inject \n, tabs, or ANSI into operator log streams.
// R60-GO-H1.
func SanitizeLogAttr(s string) string {
	return sanitizeKeyComponent(s)
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
	Backend      string             `json:"backend,omitempty"`     // "claude", "kiro", ...
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
	UserLabel    string             `json:"user_label,omitempty"`    // operator-set override for sidebar/header title
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
		Backend:    s.Backend(),
		CLIName:    s.CLIName(),
		CLIVersion: s.CLIVersion(),
		UserLabel:  s.UserLabel(),
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
		// Prefer whichever is larger: a freshly resumed process reports 0
		// until the first `result` event arrives, but s.totalCost carries
		// the historical cumulative value restored from sessions.json.
		// Claude CLI's total_cost_usd under --resume is cumulative, so once
		// the next result lands, proc.TotalCost() will be >= s.totalCost
		// and the display won't regress.
		if pc := proc.TotalCost(); pc > s.totalCost {
			snap.TotalCost = pc
		} else {
			snap.TotalCost = s.totalCost
		}
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

// EventEntriesBefore returns up to `limit` entries with Time < beforeMS
// in chronological order. Drives the dashboard "load earlier" button:
// caller passes the oldest rendered event's timestamp; server returns the
// preceding page of up to `limit` entries.
//
// beforeMS <= 0 is treated as "no upper bound" — equivalent to the tail
// of the log, matching EventLastN semantics. limit <= 0 returns nil.
func (s *ManagedSession) EventEntriesBefore(beforeMS int64, limit int) []cli.EventEntry {
	if limit <= 0 {
		return nil
	}
	proc := s.loadProcess()
	if proc != nil {
		return proc.EventEntriesBefore(beforeMS, limit)
	}
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	if len(s.persistedHistory) == 0 {
		return nil
	}
	// Walk backward collecting up to `limit` entries strictly older than
	// beforeMS. persistedHistory is not guaranteed to be sorted (see
	// EventEntriesSince), so a full linear walk is the conservative choice.
	out := make([]cli.EventEntry, 0, limit)
	// Collect newest-to-oldest into a temp and reverse; keeps order stable
	// when entries share identical Time values (common for a single batch
	// InjectHistory where timestamps collapse to the same ms).
	for i := len(s.persistedHistory) - 1; i >= 0 && len(out) < limit; i-- {
		e := s.persistedHistory[i]
		if beforeMS > 0 && e.Time >= beforeMS {
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
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
	if prompt != "" && loadStringOrEmpty(&s.lastPrompt) == "" {
		s.lastPrompt.Store(prompt)
	}
	if activity != "" && loadStringOrEmpty(&s.lastActivity) == "" {
		s.lastActivity.Store(activity)
	}
}

// loadStringOrEmpty returns the stored string or "" if never stored / stored as "".
// Avoids the pitfall where `atomic.Value.Load() == nil` misreports "already set"
// after an empty string was stored.
func loadStringOrEmpty(v *atomic.Value) string {
	if x := v.Load(); x != nil {
		if s, ok := x.(string); ok {
			return s
		}
	}
	return ""
}

// extractLastPromptFromProcess scans the attached process's event log to populate
// lastPrompt and lastActivity when they haven't been set yet (e.g. after shim reconnect
// where events were injected directly into the process, bypassing InjectHistory).
func (s *ManagedSession) extractLastPromptFromProcess() {
	if loadStringOrEmpty(&s.lastPrompt) != "" && loadStringOrEmpty(&s.lastActivity) != "" {
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
	if prompt != "" && loadStringOrEmpty(&s.lastPrompt) == "" {
		s.lastPrompt.Store(prompt)
	}
	if activity != "" && loadStringOrEmpty(&s.lastActivity) == "" {
		s.lastActivity.Store(activity)
	}
}
