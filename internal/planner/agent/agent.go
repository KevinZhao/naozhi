// Package agent runs the global Planner session, planner:main
// (docs/rfc/planner.md §15.1 P3a, §4.1): its launch spec, keeping it alive,
// the daily shift change with a handoff summary, and the entry user messages
// take to it — a deterministic snapshot before each message and a receipt
// within a second. The Planner itself is a Claude CLI session; nothing here
// calls a model directly.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	"github.com/naozhi/naozhi/internal/session/sessionview"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Launch spec (§15.1, §4.1).
const (
	Key    = sessionkey.PlannerMainKey
	Model  = "claude-opus-5-5[1m]"
	Effort = "medium"
	// PromptCacheTTL keeps the Planner's prompt cache an hour (§5.2): its
	// turns are often more than the default 5 minutes apart, and each miss
	// rewrites a context of 100k+ tokens.
	PromptCacheTTL = "1h"
)

// NameFor is the Planner's --name for the state root root: "planner-" and a
// short hash of the root's absolute path. Every naozhi on a machine shares
// CC's cross-session channel, so a fixed name would take another instance's
// reports; the hash keeps the name the same across restarts and shift
// changes, which is what the sessions it dispatched were told (D92).
func NameFor(root string) string {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	sum := sha256.Sum256([]byte(root))
	return "planner-" + hex.EncodeToString(sum[:3])
}

// Tools is the Planner's whole built-in tool set: read-only tools to check a
// claim, and CC's own cross-session channel.
var Tools = []string{"Read", "Grep", "Glob", "ListAgents", "SendMessage"}

// ReceiptText is the gateway's receipt for a user message (§4.1 先回执再干活).
const ReceiptText = "收到，正在看"

// Timing.
const (
	// interval is the reconcile period: revival and the shift-change check.
	interval = time.Minute
	// shiftHour:shiftMinute local is the daily shift change, the morning
	// brief's time.
	shiftHour, shiftMinute = 8, 30
	// handoffWait is how long the outgoing session has to write its summary;
	// past it the shift changes with none.
	handoffWait = 3 * time.Minute
	// handoffMaxRunes caps a stored summary. The prompt asks for 2,000 字
	// (§15.12 ⑧); a little over it is kept whole rather than cut mid-line.
	handoffMaxRunes = 3000
	// A conversation whose last call was sent more than sizeShiftTokens
	// (input + cache_read + cache_creation) changes shift once the Planner
	// has been idle sizeShiftIdle, at most once per sizeShiftGap since the
	// conversation started (§15.12 ⑧, D106).
	sizeShiftTokens = 250_000
	sizeShiftIdle   = 5 * time.Minute
	sizeShiftGap    = 2 * time.Hour
	// reviveBackoff doubles from its base after each failed revival, up to
	// its cap.
	reviveBackoffBase = time.Minute
	reviveBackoffMax  = 30 * time.Minute
	// maxMessageBytes caps a user message, the dashboard send's cap.
	maxMessageBytes = 1 << 20
)

// Errors Send and SetStopped report; the HTTP layer maps them to statuses.
var (
	ErrOff      = errors.New("未开启 Planner（planner.enabled）")
	ErrStopped  = errors.New("已停用 Planner")
	ErrNotReady = errors.New("正在启动 Planner，请稍后再试")
	ErrEmpty    = errors.New("消息为空")
	ErrTooLong  = errors.New("消息太长")
	ErrNoRef    = errors.New("没有这个会话")
)

// SessionState is what the router knows of planner:main.
type SessionState struct {
	Exists bool
	// Alive is a running process; a suspended session only has an ID.
	Alive bool
	// Running is a turn in flight.
	Running   bool
	SessionID string
}

// Sessions is the session router as the Planner's lifecycle uses it.
type Sessions interface {
	State(key string) SessionState
	// Ensure creates key's session, or revives it (resuming its
	// conversation), with opts.
	Ensure(ctx context.Context, key string, opts sessionview.AgentOpts) error
	// Recreate replaces key's session with a fresh conversation.
	Recreate(ctx context.Context, key string, opts sessionview.AgentOpts) error
	// Release stops key's process and keeps the session for a later resume;
	// false while a turn runs.
	Release(key string) bool
	// Label is a session's title for the snapshot, "" when unknown.
	Label(key string) string
	// Name is a session's cross-session name, "" when unknown.
	Name(key string) string
	// CompactedSince reports whether key's conversation may have been
	// compacted since t, so what it was told then may be summarised away.
	CompactedSince(key string, t time.Time) bool
}

// Turns is the gateway's turn entry for the Planner.
type Turns interface {
	// SubmitPlanner runs text and images (nil for none) from src as a turn
	// on key, spawning it with opts; said (nil for none) is the user's own
	// words in it, recorded with the request; done (nil for none) gets the
	// reply, or the error the turn ended with. queued is text waiting behind
	// a turn already running.
	SubmitPlanner(key string, src Source, said *Words, text string, images []clievent.Attachment, opts sessionview.AgentOpts, done func(reply string, err error)) (queued bool, err error)
	// PlannerTurnSources is the sources of key's requests in flight,
	// sorted (TurnSources); none between turns.
	PlannerTurnSources(key string) []Source
	// PlannerTurnWords is the user's words of key's requests in flight,
	// oldest first (TurnWords); none between turns.
	PlannerTurnWords(key string) []Words
	// PlannerReceipt shows text in key's conversation as a system event,
	// never sent to the CLI.
	PlannerReceipt(key, text string)
	// PlannerNote writes text into key's conversation history as a system
	// event, never sent to the CLI; the history is in memory.
	PlannerNote(key, text string)
	// DropPlannerQueue discards the messages queued for key's next turn.
	DropPlannerQueue(key string)
}

// Config wires an Agent.
type Config struct {
	// Enabled is planner.enabled.
	Enabled bool
	// Layout is the session store's data layout; the state root, the
	// Planner's working directory and its own state live under it.
	Layout datadir.Layout
	// Home is the user's home directory (auto-memory, credential rules).
	Home string
	// DenyPaths are more absolute paths the Planner must not read (the
	// config file, a shim state directory outside the data root).
	DenyPaths []string
	Sessions  Sessions
	// Workspaces names a workspace ID in the snapshot; nil leaves the ID.
	Workspaces *workspace.Registry
	// Journal keeps the gateway's notes in the conversation (Note) across
	// restarts; nil keeps them in memory only.
	Journal *journal.Journal
	// Waiting and Promises feed the snapshot; nil leaves their section out.
	Waiting  func() []Waiting
	Promises func() []Promise
	// Conclusion is the first sentence of a session's latest report, for
	// its snapshot line; nil shows none.
	Conclusion func(key string) string
	// ClaudeDir is the Claude CLI's directory: the Planner's transcript (its
	// context size, for the change of shift by size) and the auto-memory the
	// remember block writes. "" turns both off.
	ClaudeDir string
	// Now is the wall clock; nil is time.Now.
	Now func() time.Time
}

// Status is the Planner's state as GET /api/planner reports it.
type Status struct {
	Enabled bool   `json:"enabled"`
	Stopped bool   `json:"stopped"`
	Key     string `json:"key"`
	Alive   bool   `json:"alive"`
	// Shift is the day of the current conversation's shift (YYYY-MM-DD).
	Shift string `json:"shift,omitempty"`
	// Workspaces names the registered workspaces by ID, for the page to
	// show a workspace the Planner wrote by its ID by name.
	Workspaces map[string]string `json:"workspaces,omitempty"`
}

// persisted is agent.json.
type persisted struct {
	V       int    `json:"v"`
	Stopped bool   `json:"stopped"`
	Shift   string `json:"shift,omitempty"`
	// Prompt is the PromptVersion the current conversation started with. A
	// resumed conversation keeps its first system prompt, so after an
	// upgrade the shift changes at once instead of at the next 08:30.
	Prompt int `json:"prompt,omitempty"`
	// Handoff names a stored summary not yet handed to a new conversation
	// (its restart failed, or the send did): the shift day, or the day and
	// prompt version for a change forced by an upgrade.
	Handoff string `json:"handoff,omitempty"`
	// ShiftAt is when the current conversation started its shift: a change
	// by size waits sizeShiftGap from it.
	ShiftAt time.Time `json:"shift_at,omitzero"`
}

// Agent keeps planner:main running and takes the user's messages to it.
type Agent struct {
	cfg  Config
	wake chan struct{}
	// name is the Planner's --name (NameFor).
	name string

	mu     sync.Mutex
	state  persisted
	opts   sessionview.AgentOpts
	turns  Turns
	health func() health.Snapshot
	// failures and retryAt pace revival after failed spawns.
	failures int
	retryAt  time.Time
	// lastSnap is the last full snapshot a turn of the current
	// conversation ran with (nextSnapshot); zero once the conversation may
	// not hold it. snapGen counts the forgets: a turn submitted before one
	// records nothing at its end.
	lastSnap sentSnapshot
	snapGen  uint64
	// shifting is set through a shift change: from the handoff turn's end
	// until the new conversation starts, an idle Planner on the old
	// session ID is not where a message goes.
	shifting bool
	// spawnedAllow is the read rules the running process started with: once
	// the workspace registry moves them, an idle Planner restarts on its
	// conversation to take the new ones (§15.12 ⑧).
	spawnedAllow []string
	// ctxSeen caches the context size of the transcript as last read, by its
	// size and modification time, so an idle Planner's tail is read once.
	ctxSeen ctxReading
	// tasks lists the 任务 for the handoff's index (SetTaskIndex); nil
	// leaves the index out.
	tasks func() []TaskLine
	// memMu serializes the remember block's writes to the memory files.
	memMu sync.Mutex
}

// ctxReading is one read of the Planner transcript's context size.
type ctxReading struct {
	path    string
	size    int64
	mod     time.Time
	tokens  int64
	present bool
}

// sentSnapshot is a full snapshot as a turn ran with it: its body (the
// block without its times, renderSnapshotKey), when it was rendered, and in
// which conversation ("" when the CLI had not named it).
type sentSnapshot struct {
	body string
	at   time.Time
	sid  string
}

// New loads the Planner's state. It starts nothing: Start does, once the
// gateway's turn entry and the health monitor exist.
func New(cfg Config) *Agent {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Enabled && cfg.Layout.PlannerRoot() == "" {
		slog.Warn("planner: no state root (session.store_path); planner.enabled ignored")
		cfg.Enabled = false
	}
	a := &Agent{cfg: cfg, wake: make(chan struct{}, 1), name: NameFor(cfg.Layout.PlannerRoot())}
	a.state = loadState(cfg.Layout.PlannerAgentPath())
	a.opts = a.buildOpts()
	a.spawnedAllow = a.opts.PermissionsAllow
	return a
}

func loadState(path string) persisted {
	st := persisted{V: 1}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return st
	case err == nil:
		if err = json.Unmarshal(data, &st); err == nil {
			return st
		}
	}
	// An unreadable file may hold a stop: fail closed, the switch turns it
	// back on.
	slog.Warn("planner: state unreadable; starting stopped", "path", path, "err", err)
	return persisted{V: 1, Stopped: true}
}

func (a *Agent) saveLocked() {
	path := a.cfg.Layout.PlannerAgentPath()
	data, err := json.MarshalIndent(a.state, "", "  ")
	if err == nil {
		if err = datadir.EnsureDir(filepath.Dir(path)); err == nil {
			err = osutil.WriteFileAtomic(path, data, 0o600)
		}
	}
	if err != nil {
		slog.Warn("planner: save state", "path", path, "err", err)
	}
}

// Name is the Planner's cross-session name, the one the sessions it
// dispatches report to.
func (a *Agent) Name() string { return a.name }

// Busy reports whether a Planner turn is running.
func (a *Agent) Busy() bool { return a.cfg.Sessions.State(Key).Running }

// Opts is planner:main's spawn options. Every argv-bearing field rides in
// the shim's SpawnOverlay, so a restart rebuilds the same argv.
func (a *Agent) Opts() sessionview.AgentOpts {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.opts
}

func (a *Agent) buildOpts() sessionview.AgentOpts {
	root := a.cfg.Layout.PlannerRoot()
	return sessionview.AgentOpts{
		Model:     Model,
		Effort:    Effort,
		Backend:   "claude",
		Workspace: a.cfg.Layout.PlannerWorkDir(),
		Exempt:    true,
		// Rendered as --append-system-prompt.
		SystemPrompt:        systemPrompt(root, a.name, memoryIndexes(a.cfg.Home)),
		Tools:               slices.Clone(Tools),
		PermissionMode:      "dontAsk",
		Name:                a.name,
		StrictMCPConfig:     true,
		CrossSessionInbound: "accept",
		PermissionsAllow:    allowRules(root, a.cfg.Layout.EventsRoot(), a.cfg.Workspaces.LocalPaths()),
		PermissionsDeny:     denyRules(a.cfg.Layout.Root(), a.cfg.DenyPaths),
		PromptCacheTTL:      PromptCacheTTL,
	}
}

// absRule is the permission-rule form of a path made absolute
// ("//abs/path").
func absRule(tool, path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return tool + "(/" + filepath.ToSlash(path) + ")"
}

// allowRules are what the Planner may read outside its working directory:
// its state root, the event logs index.json points at, the "懂你" sources and
// the registered workspaces on this node, so it can check a claim against
// the files (RFC §4.1; Read rules also cover Grep and Glob). Under dontAsk
// anything else is refused; denyRules still win inside these trees. A
// workspace registered later is picked up once the Planner is next idle
// (refreshReadScope).
func allowRules(root, events string, workspaces []string) []string {
	rules := []string{
		absRule("Read", filepath.Join(root, "**")),
		absRule("Read", filepath.Join(events, "**")),
		"Read(~/.claude/CLAUDE.md)",
		"Read(~/.claude/projects/*/memory/**)",
	}
	for _, ws := range workspaces {
		if r := absRule("Read", filepath.Join(ws, "**")); !slices.Contains(rules, r) {
			rules = append(rules, r)
		}
	}
	return rules
}

// denyRules keep credentials out of reach even inside an allowed tree: the
// home-relative ones §15.1 lists, the data root's secret-bearing files (its
// config-shaped files, shim state with auth tokens, profile secrets, debug
// captures with API keys) and the extra paths.
func denyRules(dataRoot string, extra []string) []string {
	rules := []string{
		"Read(~/.aws/**)",
		"Read(~/.ssh/**)",
		"Read(~/.naozhi/*.yaml)",
		"Read(~/.naozhi/**/*.yaml)",
		"Read(~/.naozhi/*.json)",
		"Read(~/.naozhi/shims/**)",
		"Read(~/.naozhi/access-profile-secrets/**)",
		"Read(~/.gnupg/**)",
		"Read(~/.config/gh/**)",
		"Read(~/.docker/config.json)",
		"Read(~/.kube/**)",
		"Read(~/.netrc)",
		"Read(~/.git-credentials)",
		"Read(~/.npmrc)",
		"Read(~/.claude.json)",
		"Read(~/.claude/.credentials.json)",
		"Read(~/.claude/settings*.json)",
		"Read(//**/.env)",
		"Read(//**/.env.*)",
		"Read(//**/*.pem)",
		"Read(//**/*.key)",
		"Read(//**/id_rsa*)",
		"Read(//**/id_ed25519*)",
	}
	if dataRoot != "" {
		for _, p := range []string{"*.yaml", filepath.Join("**", "*.yaml"), "*.json", filepath.Join("shims", "**"),
			filepath.Join("access-profile-secrets", "**"), filepath.Join("session-settings", "**"), filepath.Join("cli-debug", "**")} {
			rules = append(rules, absRule("Read", filepath.Join(dataRoot, p)))
		}
	}
	for _, p := range extra {
		if p == "" {
			continue
		}
		rules = append(rules, absRule("Read", p))
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			rules = append(rules, absRule("Read", filepath.Join(p, "**")))
		}
	}
	return rules
}

// maxMemoryIndexes caps the auto-memory indexes listed in the prompt.
const maxMemoryIndexes = 30

// memoryIndexes lists the auto-memory indexes under home, home-relative.
func memoryIndexes(home string) []string {
	if home == "" {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "memory", "MEMORY.md"))
	slices.Sort(matches)
	if len(matches) > maxMemoryIndexes {
		matches = matches[:maxMemoryIndexes]
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if rel, err := filepath.Rel(home, m); err == nil {
			out = append(out, "~/"+filepath.ToSlash(rel))
		}
	}
	return out
}

// Start runs the lifecycle until ctx ends: it keeps planner:main alive and
// changes its shift each morning. With planner.enabled off it only stops a
// Planner an earlier run left alive.
func (a *Agent) Start(ctx context.Context, turns Turns, snapshot func() health.Snapshot) {
	if !a.cfg.Enabled {
		if st := a.cfg.Sessions.State(Key); st.Alive && a.cfg.Sessions.Release(Key) {
			slog.Info("planner: disabled; stopped the running Planner")
		}
		return
	}
	if err := datadir.EnsureDir(a.cfg.Layout.PlannerWorkDir()); err != nil {
		slog.Warn("planner: working directory unusable; not starting", "err", err)
		return
	}
	a.mu.Lock()
	a.turns, a.health = turns, snapshot
	a.mu.Unlock()
	go a.loop(ctx)
}

func (a *Agent) loop(ctx context.Context) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		a.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.wake:
		}
	}
}

func (a *Agent) poke() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// reconcile is one lifecycle step: stop when stopped, else revive a dead
// Planner (with backoff) and change the shift when it is due.
func (a *Agent) reconcile(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("planner: reconcile panicked", "panic", r)
		}
	}()
	now := a.cfg.Now()
	a.mu.Lock()
	stopped, opts := a.state.Stopped, a.opts
	a.mu.Unlock()
	if stopped {
		if a.cfg.Sessions.State(Key).Alive {
			a.cfg.Sessions.Release(Key)
		}
		return
	}
	st := a.cfg.Sessions.State(Key)
	if !st.Alive {
		// A revival may not resume the conversation (its resume target
		// lost): the next message carries a full snapshot.
		a.forgetSnapshot()
		if !a.revive(ctx, now, opts) {
			return
		}
		if st.SessionID == "" {
			// A fresh conversation is today's shift already.
			a.markShift(now)
		}
	}
	a.mu.Lock()
	shift, pending, prompt := a.state.Shift, a.state.Handoff, a.state.Prompt
	a.mu.Unlock()
	switch {
	case shift == "":
		a.markShift(now)
	case shift != shiftDay(now):
		// A busy Planner changes at a later reconcile, so the handoff
		// request runs at once rather than queued behind a turn.
		if !st.Running {
			a.changeShift(ctx, now, shiftDay(now))
		}
	case prompt != PromptVersion:
		// An upgrade changed the system prompt; the resumed conversation
		// would keep the old one until tomorrow. A fresh conversation was
		// marked above, so this is one started on the old prompt, even when
		// its session ID is unknown (a shim adopted before its first turn).
		if !st.Running {
			a.changeShift(ctx, now, fmt.Sprintf("%s-v%d", shiftDay(now), PromptVersion))
		}
	case pending != "":
		a.sendHandoff(pending, nil, opts)
	case !st.Running && a.oversized(now, st.SessionID):
		a.changeShift(ctx, now, a.sizeShiftName(now))
	default:
		a.refreshReadScope(st)
	}
}

// oversized reports whether the conversation sessionID is due a change of
// shift by size: its last call's context past sizeShiftTokens, its
// transcript untouched for sizeShiftIdle and its shift sizeShiftGap old.
func (a *Agent) oversized(now time.Time, sessionID string) bool {
	if sessionID == "" || a.cfg.ClaudeDir == "" {
		return false
	}
	a.mu.Lock()
	since := a.state.ShiftAt
	a.mu.Unlock()
	if !since.IsZero() && now.Sub(since) < sizeShiftGap {
		return false
	}
	path := claudefs.LocateSessionJSONL(a.cfg.ClaudeDir, a.cfg.Layout.PlannerWorkDir(), sessionID)
	fi, err := os.Stat(path)
	if err != nil || now.Sub(fi.ModTime()) < sizeShiftIdle {
		return false
	}
	a.mu.Lock()
	seen := a.ctxSeen
	a.mu.Unlock()
	if seen.path != path || seen.size != fi.Size() || !seen.mod.Equal(fi.ModTime()) {
		tokens, found, err := claudefs.LastContextTokens(path)
		if err != nil {
			slog.Warn("planner: read context size", "path", path, "err", err)
			return false
		}
		seen = ctxReading{path: path, size: fi.Size(), mod: fi.ModTime(), tokens: tokens, present: found}
		a.mu.Lock()
		a.ctxSeen = seen
		a.mu.Unlock()
	}
	if seen.present && seen.tokens > sizeShiftTokens {
		slog.Info("planner: context past the size limit; changing shift", "tokens", seen.tokens)
		return true
	}
	return false
}

// sizeShiftName names a change by size: the shift day and the first
// "-c<n>" no stored summary has.
func (a *Agent) sizeShiftName(now time.Time) string {
	day := shiftDay(now)
	for n := 1; ; n++ {
		name := fmt.Sprintf("%s-c%d", day, n)
		if _, err := os.Stat(a.handoffPath(name)); errors.Is(err, os.ErrNotExist) || n >= 99 {
			return name
		}
	}
}

// refreshReadScope keeps the Planner's read rules in step with the
// workspace registry: new rules go into the spawn options at once, and a
// live process started on others is released while idle, to be resumed on
// its conversation with them at the next reconcile.
func (a *Agent) refreshReadScope(st SessionState) {
	want := allowRules(a.cfg.Layout.PlannerRoot(), a.cfg.Layout.EventsRoot(), a.cfg.Workspaces.LocalPaths())
	a.mu.Lock()
	if !slices.Equal(a.opts.PermissionsAllow, want) {
		a.opts.PermissionsAllow = want
	}
	stale := !slices.Equal(a.spawnedAllow, want)
	shifting := a.shifting
	a.mu.Unlock()
	if !stale || !st.Alive || st.Running || shifting {
		return
	}
	if a.cfg.Sessions.Release(Key) {
		slog.Info("planner: workspaces changed; restarting on the conversation to read them", "rules", len(want))
		a.poke()
	}
}

// revive spawns or resumes planner:main, pacing retries after failures.
func (a *Agent) revive(ctx context.Context, now time.Time, opts sessionview.AgentOpts) bool {
	a.mu.Lock()
	wait := now.Before(a.retryAt)
	a.mu.Unlock()
	if wait {
		return false
	}
	if err := a.cfg.Sessions.Ensure(ctx, Key, opts); err != nil {
		a.failed(now, "planner: start failed", err)
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.spawnedAllow = opts.PermissionsAllow
	if a.failures > 0 {
		slog.Info("planner: started after failures", "failures", a.failures)
	}
	a.failures, a.retryAt = 0, time.Time{}
	return true
}

// failed records a failed spawn and backs the next revival off.
func (a *Agent) failed(now time.Time, msg string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures++
	backoff := min(reviveBackoffBase<<min(a.failures-1, 5), reviveBackoffMax)
	a.retryAt = now.Add(backoff)
	slog.Warn(msg, "err", err, "failures", a.failures, "retry_in", backoff)
}

// shiftDay is the shift t belongs to: a day's shift starts at 08:30 local,
// so before then t is still in the previous day's.
func shiftDay(t time.Time) string {
	start := time.Date(t.Year(), t.Month(), t.Day(), shiftHour, shiftMinute, 0, 0, t.Location())
	if t.Before(start) {
		t = t.AddDate(0, 0, -1)
	}
	return t.Format(time.DateOnly)
}

// markShift records that the current conversation is now's shift and runs
// on the current system prompt.
func (a *Agent) markShift(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Shift = shiftDay(now)
	a.state.Prompt = PromptVersion
	a.state.ShiftAt = now
	a.saveLocked()
}

func (a *Agent) handoffPath(day string) string {
	return filepath.Join(a.cfg.Layout.PlannerHandoffRoot(), day+".md")
}

// changeShift has the outgoing session write a handoff summary, retires it,
// and starts a fresh conversation whose first message carries the summary
// and the 任务 index. day names the summary: the shift day, day and prompt
// version for a change an upgrade forces, or day and "-c<n>" for one by
// size. A summary already stored under that name is
// reused rather than asked for again. It stays pending until sent, so after
// a failed restart the conversation revival starts still gets it.
func (a *Agent) changeShift(ctx context.Context, now time.Time, day string) {
	// From here every message carries a full snapshot: once the handoff
	// turn ends the Planner is idle on the old session ID, while a message
	// queued then runs in the new conversation. What a turn on the old
	// session ran with is not in the new one, and a turn submitted before
	// may end only after, its conversation named by the new one's ID: the
	// forget at the end makes those record nothing.
	a.mu.Lock()
	a.shifting = true
	a.mu.Unlock()
	a.forgetSnapshot()
	defer func() {
		a.forgetSnapshot()
		a.mu.Lock()
		a.shifting = false
		a.mu.Unlock()
	}()
	path := a.handoffPath(day)
	summary, err := os.ReadFile(path)
	if err != nil {
		text := a.askHandoff(ctx)
		if ctx.Err() != nil {
			return // shutting down: the next start changes the shift
		}
		summary = []byte(text)
		if text != "" {
			if err := datadir.EnsureDir(filepath.Dir(path)); err != nil {
				slog.Warn("planner: handoff directory", "err", err)
			} else if err := osutil.WriteFileAtomic(path, summary, 0o600); err != nil {
				slog.Warn("planner: save handoff", "path", path, "err", err)
			}
		}
	}
	a.mu.Lock()
	// The memory indexes and prompt are re-read for the new conversation.
	a.opts = a.buildOpts()
	opts := a.opts
	a.state.Handoff = ""
	if len(summary) > 0 {
		a.state.Handoff = day
	}
	a.saveLocked()
	a.mu.Unlock()
	if err := a.cfg.Sessions.Recreate(ctx, Key, opts); err != nil {
		// The old session is gone: revival starts the new one, and the
		// pending summary follows it.
		a.failed(now, "planner: shift change: start new session failed", err)
		return
	}
	a.mu.Lock()
	a.spawnedAllow = opts.PermissionsAllow
	a.mu.Unlock()
	a.markShift(now)
	slog.Info("planner: shift changed", "day", day, "handoff_bytes", len(summary))
	if len(summary) > 0 {
		a.sendHandoff(day, summary, opts)
	}
}

// sendHandoff hands day's summary to the current conversation, reading it
// from its file when summary is nil, and clears it as pending once
// submitted.
func (a *Agent) sendHandoff(day string, summary []byte, opts sessionview.AgentOpts) {
	if summary == nil {
		var err error
		if summary, err = os.ReadFile(a.handoffPath(day)); err != nil {
			slog.Warn("planner: pending handoff unreadable; dropped", "day", day, "err", err)
		}
	}
	if len(summary) > 0 {
		a.mu.Lock()
		turns := a.turns
		a.mu.Unlock()
		if _, err := turns.SubmitPlanner(Key, SourceShift, nil, handoffMessage(day, string(summary), a.taskIndex()), nil, opts, nil); err != nil {
			slog.Warn("planner: send handoff to new session", "err", err)
			return
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.Handoff == day {
		a.state.Handoff = ""
		a.saveLocked()
	}
}

// askHandoff asks the outgoing session for its summary and waits up to
// handoffWait; "" on a timeout or failure.
func (a *Agent) askHandoff(ctx context.Context) string {
	if a.cfg.Sessions.State(Key).SessionID == "" {
		return "" // nothing was said yet
	}
	a.mu.Lock()
	turns, opts := a.turns, a.opts
	a.mu.Unlock()
	type reply struct {
		text string
		err  error
	}
	ch := make(chan reply, 1)
	queued, err := turns.SubmitPlanner(Key, SourceShift, nil, handoffPrompt, nil, opts, func(text string, err error) {
		ch <- reply{text, err}
	})
	if err != nil {
		slog.Warn("planner: ask for handoff", "err", err)
		return ""
	}
	t := time.NewTimer(handoffWait)
	defer t.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			slog.Warn("planner: handoff turn failed", "err", r.err)
			return ""
		}
		return textutil.TruncateRunes(strings.TrimSpace(r.text), handoffMaxRunes)
	case <-t.C:
		slog.Warn("planner: handoff timed out; changing shift without one")
	case <-ctx.Done():
	}
	// A request that never started would otherwise run first on the new
	// conversation once the restart frees the queue.
	if queued {
		turns.DropPlannerQueue(Key)
	}
	return ""
}

// Send takes a user message to the Planner: it puts the snapshot before the
// text, submits the turn and shows the receipt. ref is a session the user
// hands over with it (交给 Planner), "" for none; its block goes before the
// snapshot, and with one the text may be empty. images are the pictures
// sent with it, which also make an empty text a message; they ride the turn
// as the CLI's content blocks, so the Planner sees them without a tool.
func (a *Agent) Send(text, ref string, images []clievent.Attachment) error {
	if !a.cfg.Enabled {
		return ErrOff
	}
	a.mu.Lock()
	turns, stopped, opts := a.turns, a.state.Stopped, a.opts
	a.mu.Unlock()
	switch text = strings.TrimSpace(text); {
	case stopped:
		return ErrStopped
	case turns == nil:
		return ErrNotReady
	case text == "" && ref == "" && len(images) == 0:
		return ErrEmpty
	case len(text) > maxMessageBytes:
		return ErrTooLong
	}
	// A handed-over session leads: the logged user bubble keeps only the
	// first EventDetailMaxRunes of the message, which the snapshot alone may
	// fill.
	head, src := "", SourceUser
	if ref != "" {
		src = SourceHandover
		if ref == Key || !a.cfg.Sessions.State(ref).Exists {
			return ErrNoRef
		}
		head = sessionRef(ref, a.cfg.Sessions.Name(ref), a.cfg.Sessions.Label(ref)) + "\n"
	}
	// The words are the user's own, without the snapshot or the ref: what
	// a relay quotes (D98).
	var said *Words
	if text != "" {
		said = &Words{Text: text, At: time.Now()}
	}
	snap, ran := a.nextSnapshot()
	if _, err := turns.SubmitPlanner(Key, src, said, strings.TrimSpace(head+snap+"\n\n"+text), images, opts, ran); err != nil {
		return err
	}
	turns.PlannerReceipt(Key, ReceiptText)
	return nil
}

// Tell sends the gateway's own message to the Planner: the outcome of an
// action block (a dispatch, the user's answer). It carries no snapshot and
// gets no receipt.
func (a *Agent) Tell(text string) error { return a.tell(SourceReceipt, text) }

// TellRefused is Tell for the receipt of action blocks the gateway refused
// in a turn outside content brought in: the receipt's own turn answers that
// content, so it is from SourceRefusal and is held to the same limits.
func (a *Agent) TellRefused(text string) error { return a.tell(SourceRefusal, text) }

// TellUnrun is Tell for the receipt of the action blocks a reply's turn did
// not run, from SourceUnrun so the receipt's own turn is known (UnrunTurn).
func (a *Agent) TellUnrun(text string) error { return a.tell(SourceUnrun, text) }

// TellAnswer is Tell for the user's answer to a question: a turn the user
// started, w its words, so the Planner may pass the answer on (D98).
func (a *Agent) TellAnswer(text string, w Words) error { return a.tellSaid(SourceAnswer, &w, text) }

func (a *Agent) tell(src Source, text string) error { return a.tellSaid(src, nil, text) }

func (a *Agent) tellSaid(src Source, said *Words, text string) error {
	if !a.cfg.Enabled {
		return ErrOff
	}
	a.mu.Lock()
	turns, stopped, opts := a.turns, a.state.Stopped, a.opts
	a.mu.Unlock()
	switch {
	case stopped:
		return ErrStopped
	case turns == nil:
		return ErrNotReady
	}
	_, err := turns.SubmitPlanner(Key, src, said, "<naozhi-receipt>\n"+strings.TrimSpace(text)+"\n</naozhi-receipt>", nil, opts, nil)
	return err
}

// nextSnapshot is the snapshot block the next message carries: the full
// one, or only a mark when what the last full one of this conversation said
// still holds. ran, the message's turn's end (nil with the mark), records a
// full one once its turn ran: a message dropped from the queue, or whose
// turn failed, may never have reached the CLI.
func (a *Agent) nextSnapshot() (text string, ran func(string, error)) {
	in := a.snapshotInput()
	full, body := renderSnapshotKey(in)
	st := a.cfg.Sessions.State(Key)
	now := in.Now
	a.mu.Lock()
	prev, gen, shifting := a.lastSnap, a.snapGen, a.shifting
	a.mu.Unlock()
	// The mark goes only to a Planner idle in the conversation the last
	// full one went to, so the message goes in as it is decided: one queued
	// behind a turn may reach a conversation a shift change or a revival
	// started by then. A dead process's spawn may not resume; an unnamed
	// conversation cannot be told from another; a compaction may have
	// summarised the last one away.
	if !shifting && st.Alive && !st.Running && prev.body == body && prev.sid != "" && prev.sid == st.SessionID &&
		!a.cfg.Sessions.CompactedSince(Key, prev.at) {
		return unchangedSnapshot(prev.at.In(now.Location()), now), nil
	}
	return full, func(_ string, err error) {
		if err != nil {
			return
		}
		sid := a.cfg.Sessions.State(Key).SessionID
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.snapGen == gen {
			a.lastSnap = sentSnapshot{body: body, at: now, sid: sid}
		}
	}
}

// forgetSnapshot makes the next message carry a full snapshot, and the
// turns in flight record none.
func (a *Agent) forgetSnapshot() {
	a.mu.Lock()
	a.lastSnap = sentSnapshot{}
	a.snapGen++
	a.mu.Unlock()
}

// Snapshot renders the full snapshot the next message would carry.
func (a *Agent) Snapshot() string { return renderSnapshot(a.snapshotInput()) }

func (a *Agent) snapshotInput() snapshotInput {
	a.mu.Lock()
	snap := a.health
	a.mu.Unlock()
	in := snapshotInput{Now: a.cfg.Now(), Label: a.cfg.Sessions.Label, Name: a.cfg.Sessions.Name, Conclusion: a.cfg.Conclusion}
	if snap != nil {
		in.Units = snap().Units
	}
	if a.cfg.Waiting != nil {
		in.Waiting = orEmpty(a.cfg.Waiting())
	}
	if a.cfg.Promises != nil {
		in.Promises = orEmpty(a.cfg.Promises())
	}
	if names := a.workspaceNames(); names != nil {
		in.Workspace = func(id string) string { return names[id] }
	}
	return in
}

// workspaceNames maps the registered workspaces' IDs to their names, nil
// without a registry.
func (a *Agent) workspaceNames() map[string]string {
	reg := a.cfg.Workspaces
	if reg == nil {
		return nil
	}
	names := map[string]string{}
	for _, w := range reg.List() {
		names[w.ID] = w.Name
	}
	return names
}

// orEmpty keeps a wired source's empty answer distinct from an unwired one.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// Enabled reports whether planner.enabled turned the Planner on; a runtime
// stop leaves it enabled.
func (a *Agent) Enabled() bool { return a != nil && a.cfg.Enabled }

// Status reports the switch and the session.
func (a *Agent) Status() Status {
	a.mu.Lock()
	st := Status{Enabled: a.cfg.Enabled, Stopped: a.state.Stopped, Key: Key, Shift: a.state.Shift}
	a.mu.Unlock()
	st.Alive = a.cfg.Sessions.State(Key).Alive
	st.Workspaces = a.workspaceNames()
	return st
}

// SetStopped is the runtime switch, persisted in the state root. Stopping
// ends an idle process at once, a busy one once its turn is over, and keeps
// the conversation; resuming revives it at once.
func (a *Agent) SetStopped(stopped bool) error {
	if !a.cfg.Enabled {
		return ErrOff
	}
	a.mu.Lock()
	a.state.Stopped = stopped
	a.failures, a.retryAt = 0, time.Time{}
	a.saveLocked()
	a.mu.Unlock()
	slog.Info("planner: switched", "stopped", stopped)
	// Released here so the Status the switch answers with is its outcome.
	if stopped && a.cfg.Sessions.State(Key).Alive {
		a.cfg.Sessions.Release(Key)
	}
	a.poke()
	return nil
}

// Note sizes: a note's cap (a promise's text is at most 500 runes), and how
// far back and how many Notes returns.
const (
	noteBytes     = 2048
	notesLookback = 7 * 24 * time.Hour
	notesMax      = 200
)

// Note is a line the gateway wrote into the Planner conversation for the
// user: a due reminder, a dispatch's outcome, a refused action block.
type Note struct {
	Time time.Time `json:"time"`
	Text string    `json:"text"`
}

type noteData struct {
	Text string `json:"text"`
}

// Note shows text in the conversation, never sent to the CLI, and journals
// it: the conversation's history is in memory, so the Planner page merges
// the journaled notes back in (Notes) after a restart.
func (a *Agent) Note(text string) {
	text = osutil.SanitizeForLog(text, noteBytes)
	if text == "" {
		return
	}
	if j := a.cfg.Journal; j != nil {
		data, err := json.Marshal(noteData{Text: text})
		if err == nil {
			err = j.Append(journal.Entry{TS: a.cfg.Now(), Kind: journal.KindDialog, Unit: Key, Data: data})
		}
		if err != nil {
			slog.Warn("planner: journal a note", "err", err)
		}
	}
	a.mu.Lock()
	turns := a.turns
	a.mu.Unlock()
	if turns != nil {
		turns.PlannerNote(Key, text)
	}
}

// Notes returns the journaled notes of the last notesLookback, oldest first,
// at most notesMax.
func (a *Agent) Notes() ([]Note, error) {
	out := []Note{}
	if a.cfg.Journal == nil {
		return out, nil
	}
	now := a.cfg.Now()
	entries, err := a.cfg.Journal.Read(now.Add(-notesLookback), now.Add(time.Minute))
	for _, e := range entries {
		var d noteData
		if e.Kind != journal.KindDialog || e.Unit != Key || json.Unmarshal(e.Data, &d) != nil || d.Text == "" {
			continue
		}
		out = append(out, Note{Time: e.TS, Text: d.Text})
	}
	if len(out) > notesMax {
		out = out[len(out)-notesMax:]
	}
	return out, err
}
