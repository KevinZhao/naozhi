// Package agent runs the global Planner session, planner:main
// (docs/rfc/planner.md §15.1 P3a, §4.1): its launch spec, keeping it alive,
// the daily shift change with a handoff summary, and the entry user messages
// take to it — a deterministic snapshot before each message and a receipt
// within a second. The Planner itself is a Claude CLI session; nothing here
// calls a model directly.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	"github.com/naozhi/naozhi/internal/session/sessionview"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Launch spec (§15.1, §4.1).
const (
	Key    = sessionkey.PlannerMainKey
	Name   = "planner"
	Model  = "claude-opus-5-5[1m]"
	Effort = "medium"
)

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
	// handoffMaxRunes caps a stored summary (the prompt asks for 800 字).
	handoffMaxRunes = 4000
	// reviveBackoff doubles from its base after each failed revival, up to
	// its cap.
	reviveBackoffBase = time.Minute
	reviveBackoffMax  = 30 * time.Minute
	// maxMessageBytes caps a user message, the dashboard send's cap.
	maxMessageBytes = 1 << 20
)

// Errors Send and SetStopped report; the HTTP layer maps them to statuses.
var (
	ErrOff      = errors.New("Planner 未开启（planner.enabled）")
	ErrStopped  = errors.New("Planner 已停用")
	ErrNotReady = errors.New("Planner 还在启动，请稍后再试")
	ErrEmpty    = errors.New("消息为空")
	ErrTooLong  = errors.New("消息太长")
)

// SessionState is what the router knows of planner:main.
type SessionState struct {
	Exists bool
	// Alive is a running process; a suspended session only has an ID.
	Alive     bool
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
}

// Turns is the gateway's turn entry for the Planner.
type Turns interface {
	// SubmitPlanner runs text as a turn on key, spawning it with opts;
	// done (nil for none) gets the reply, or the error the turn ended with.
	SubmitPlanner(key, text string, opts sessionview.AgentOpts, done func(reply string, err error)) error
	// PlannerReceipt shows text in key's conversation as a system event,
	// never sent to the CLI.
	PlannerReceipt(key, text string)
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
	// Waiting and Promises feed the snapshot; nil leaves their section out.
	Waiting  func() []Waiting
	Promises func() []Promise
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
}

// persisted is agent.json.
type persisted struct {
	V       int    `json:"v"`
	Stopped bool   `json:"stopped"`
	Shift   string `json:"shift,omitempty"`
}

// Agent keeps planner:main running and takes the user's messages to it.
type Agent struct {
	cfg  Config
	wake chan struct{}

	mu     sync.Mutex
	state  persisted
	opts   sessionview.AgentOpts
	turns  Turns
	health func() health.Snapshot
	// failures and retryAt pace revival after failed spawns.
	failures int
	retryAt  time.Time
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
	a := &Agent{cfg: cfg, wake: make(chan struct{}, 1)}
	a.state = loadState(cfg.Layout.PlannerAgentPath())
	a.opts = a.buildOpts()
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
		SystemPrompt:        systemPrompt(root, memoryIndexes(a.cfg.Home)),
		Tools:               slices.Clone(Tools),
		PermissionMode:      "dontAsk",
		Name:                Name,
		StrictMCPConfig:     true,
		CrossSessionInbound: "accept",
		PermissionsAllow:    allowRules(root, a.cfg.Layout.EventsRoot()),
		PermissionsDeny:     denyRules(a.cfg.Layout.Root(), a.cfg.DenyPaths),
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
// its state root, the event logs index.json points at, and the "懂你"
// sources. Under dontAsk anything else is refused.
func allowRules(root, events string) []string {
	return []string{
		absRule("Read", filepath.Join(root, "**")),
		absRule("Read", filepath.Join(events, "**")),
		"Read(~/.claude/CLAUDE.md)",
		"Read(~/.claude/projects/*/memory/**)",
	}
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
		if !a.revive(ctx, now, opts) {
			return
		}
		if st.SessionID == "" {
			// A fresh conversation is today's shift already.
			a.markShift(now)
		}
	}
	a.mu.Lock()
	shift := a.state.Shift
	a.mu.Unlock()
	switch {
	case shift == "":
		a.markShift(now)
	case shift != shiftDay(now):
		a.changeShift(ctx, now)
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
	err := a.cfg.Sessions.Ensure(ctx, Key, opts)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.failures++
		backoff := min(reviveBackoffBase<<min(a.failures-1, 5), reviveBackoffMax)
		a.retryAt = now.Add(backoff)
		slog.Warn("planner: start failed", "err", err, "failures", a.failures, "retry_in", backoff)
		return false
	}
	if a.failures > 0 {
		slog.Info("planner: started after failures", "failures", a.failures)
	}
	a.failures, a.retryAt = 0, time.Time{}
	return true
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

func (a *Agent) markShift(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Shift = shiftDay(now)
	a.saveLocked()
}

// changeShift has the outgoing session write a handoff summary, retires it,
// and starts a fresh conversation whose first message carries the summary.
// A summary already stored for this shift (a retry after a failed restart)
// is reused rather than asked for again.
func (a *Agent) changeShift(ctx context.Context, now time.Time) {
	day := shiftDay(now)
	path := filepath.Join(a.cfg.Layout.PlannerHandoffRoot(), day+".md")
	summary, err := os.ReadFile(path)
	if err != nil {
		text := a.askHandoff(ctx)
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
	opts, turns := a.opts, a.turns
	a.mu.Unlock()
	if err := a.cfg.Sessions.Recreate(ctx, Key, opts); err != nil {
		slog.Warn("planner: shift change: start new session", "err", err)
		return
	}
	a.markShift(now)
	slog.Info("planner: shift changed", "day", day, "handoff_bytes", len(summary))
	if len(summary) > 0 {
		if err := turns.SubmitPlanner(Key, handoffMessage(day, string(summary)), opts, nil); err != nil {
			slog.Warn("planner: send handoff to new session", "err", err)
		}
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
	if err := turns.SubmitPlanner(Key, handoffPrompt, opts, func(text string, err error) {
		ch <- reply{text, err}
	}); err != nil {
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
	return ""
}

// Send takes a user message to the Planner: it puts the snapshot before the
// text, submits the turn and shows the receipt.
func (a *Agent) Send(text string) error {
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
	case text == "":
		return ErrEmpty
	case len(text) > maxMessageBytes:
		return ErrTooLong
	}
	if err := turns.SubmitPlanner(Key, a.Snapshot()+"\n\n"+text, opts, nil); err != nil {
		return err
	}
	turns.PlannerReceipt(Key, ReceiptText)
	return nil
}

// Snapshot renders the snapshot the next message would carry.
func (a *Agent) Snapshot() string {
	a.mu.Lock()
	snap := a.health
	a.mu.Unlock()
	in := snapshotInput{Now: a.cfg.Now(), Label: a.cfg.Sessions.Label}
	if snap != nil {
		in.Units = snap().Units
	}
	if a.cfg.Waiting != nil {
		in.Waiting = orEmpty(a.cfg.Waiting())
	}
	if a.cfg.Promises != nil {
		in.Promises = orEmpty(a.cfg.Promises())
	}
	if reg := a.cfg.Workspaces; reg != nil {
		names := map[string]string{}
		for _, w := range reg.List() {
			names[w.ID] = w.Name
		}
		in.Workspace = func(id string) string { return names[id] }
	}
	return renderSnapshot(in)
}

// orEmpty keeps a wired source's empty answer distinct from an unwired one.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// Status reports the switch and the session.
func (a *Agent) Status() Status {
	a.mu.Lock()
	st := Status{Enabled: a.cfg.Enabled, Stopped: a.state.Stopped, Key: Key, Shift: a.state.Shift}
	a.mu.Unlock()
	st.Alive = a.cfg.Sessions.State(Key).Alive
	return st
}

// SetStopped is the runtime switch, persisted in the state root. Stopping
// ends the process once its turn is over and keeps the conversation;
// resuming revives it at once.
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
	a.poke()
	return nil
}
