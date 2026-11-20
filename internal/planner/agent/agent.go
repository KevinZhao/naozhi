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
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/apierr"
	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/planner/front"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/parked"
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

// ReceiptText is the gateway's receipt for a user message (§4.1 先回执再干活).
const ReceiptText = "收到，正在看"

// Timing.
const (
	// interval is the reconcile period: revival and the shift-change check.
	interval = time.Minute
	// shiftHour:shiftMinute local is where a shift's day starts (shiftDay),
	// the morning brief's time. The shift itself changes only by size or by
	// prompt version (D130).
	shiftHour, shiftMinute = 8, 30
	// handoffWait is how long the outgoing session has to write its summary.
	handoffWait = 3 * time.Minute
	// A change of shift whose summary could not be written is put off
	// handoffRetry and tried again, the conversation kept. While the model
	// cannot be reached (handoffUnreachable) that goes on however long: a
	// credential expired overnight comes back in the morning. A turn that
	// failed, timed out or said nothing waits handoffRetry longer each time,
	// and the handoffTries-th in a row changes the shift with none, so a
	// conversation that cannot answer at all is still replaced.
	handoffRetry = 10 * time.Minute
	handoffTries = 3
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
	// Fronts feeds the snapshot the lines of work in progress (§15.17); nil
	// leaves the section out.
	Fronts func() []front.Front
	// Conclusion is the first sentence of a session's latest report, for
	// its snapshot line; nil shows none.
	Conclusion func(key string) string
	// MCPConfig is cli.mcp_config's absolute path: the Planner gets the read
	// tools of the servers in it that it knows (tools.go). "" gives it none.
	MCPConfig string
	// OwnSettings is whether the session spawns on the naozhi-owned settings
	// file (naozhi_settings.enabled): only then are the Planner's allow rules
	// its whole grant, and only then does it get Bash for gh's reads. With
	// the CLI loading ~/.claude/settings.json itself, an allow rule there
	// (Bash(aws *)) would open with it.
	OwnSettings bool
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
	// Running is a turn in flight: the page says 处理汇报中 while it is one
	// you did not start (§15.14 ②), a turn cut off without a result
	// included.
	Running bool `json:"running"`
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
	// upgrade the shift changes at once.
	Prompt int `json:"prompt,omitempty"`
	// Handoff names a stored summary not yet handed to a new conversation
	// (its restart failed, or the send did): the shift day, or the day and
	// prompt version for a change forced by an upgrade.
	Handoff string `json:"handoff,omitempty"`
	// ShiftAt is when the current conversation started its shift: a change
	// by size waits sizeShiftGap from it.
	ShiftAt time.Time `json:"shift_at,omitzero"`
	// Scope fingerprints the scope the live process started with (scope.sum).
	// Kept here because a restart of naozhi keeps a process a shim holds:
	// what it started with is not what this binary would start it with after
	// an upgrade that moved the tools or the rules.
	Scope string `json:"scope,omitempty"`
	// Held is the receipts waiting for the next one (held.go), kept so a
	// restart loses none.
	Held []heldReceipt `json:"held,omitempty"`
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
	// cur is the scope the spawn options carry; state.Scope fingerprints the
	// one the running process started with. Once the workspace registry or
	// cli.mcp_config moves cur, an idle Planner restarts on its conversation
	// to take it (§15.12 ⑧).
	cur scope
	// handoffFails counts the changes of shift put off in a row for want of
	// a summary; until shiftRetryAt none is tried again.
	handoffFails int
	shiftRetryAt time.Time
	// ctxSeen caches the context size of the transcript as last read, by its
	// size and modification time, so an idle Planner's tail is read once.
	ctxSeen ctxReading
	// tasks lists the 任务 for the handoff's index (SetTaskIndex); nil
	// leaves the index out.
	tasks func() []TaskLine
	// routines lists the standing tasks for the snapshot (SetRoutines); nil
	// leaves the section out.
	routines func() []Routine
	// parked is the 搁置区 the handoff check and the park block move the
	// "等你定" items into (§15.14 ⑤).
	parked *parked.Store
	// tellMu serializes Tell, from the held receipts read to their drop.
	tellMu sync.Mutex
	// userAt is when the user last sent a message on the Planner page
	// (Send): progress-class receipts wait chatQuiet from it (TellLater).
	// Not kept across a restart: the first idle Planner after one gets what
	// was held.
	userAt time.Time
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
	a := &Agent{cfg: cfg, wake: make(chan struct{}, 1), name: NameFor(cfg.Layout.PlannerRoot()),
		parked: parked.New(cfg.Layout.PlannerParkedPath(), cfg.Layout.PlannerDecidePath(), cfg.Now)}
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

// buildOpts is the launch spec with the scope as it is now, which becomes
// a.cur. The caller holds a.mu or is New.
func (a *Agent) buildOpts() sessionview.AgentOpts {
	root := a.cfg.Layout.PlannerRoot()
	opts := sessionview.AgentOpts{
		Model:     Model,
		Effort:    Effort,
		Backend:   "claude",
		Workspace: a.cfg.Layout.PlannerWorkDir(),
		Exempt:    true,
		// Rendered as --append-system-prompt.
		SystemPrompt:   systemPrompt(root, a.name, memoryIndexes(a.cfg.Home)),
		PermissionMode: "dontAsk",
		Name:           a.name,
		// Only the servers of the scope's own --mcp-config, never the
		// router-wide cli.mcp_config with its write tools' servers whole.
		StrictMCPConfig:     true,
		CrossSessionInbound: "accept",
		PromptCacheTTL:      PromptCacheTTL,
	}
	a.cur = a.scope()
	a.cur.apply(&opts)
	return opts
}

// absRule is the permission-rule form of a path made absolute
// ("//abs/path").
func absRule(tool, path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return tool + "(/" + filepath.ToSlash(path) + ")"
}

// readRules are the Planner's allow and deny rules, both built on one read
// of the workspace aliases so a spelling the allow rules open is one the
// deny rules close.
func (a *Agent) readRules() (allow, deny []string) {
	aliases := a.cfg.Workspaces.LocalAliases()
	allow = allowRules(a.cfg.Layout.PlannerRoot(), a.cfg.Layout.EventsRoot(), a.cfg.Workspaces.LocalPaths(), aliases)
	return allow, a.denyRules(aliases)
}

// allowRules are what the Planner may read outside its working directory:
// the user's home (D129: a project not registered as a workspace, the
// transcripts, the "懂你" sources), its state root and the event logs
// index.json points at, and the registered workspaces on this node, which
// may lie outside home (RFC §4.1; Read rules also cover Grep and Glob).
// Under dontAsk anything else is refused; denyRules still win inside these
// trees. A workspace registered later is picked up once the Planner is next
// idle (refreshScope). A read through a symlink is not allowed by the rule
// for the resolved directory alone (the CLI checks the spelling as written),
// so a workspace's aliases (the spellings sessions reached it by, as the
// snapshot and reports give it) get rules of their own, after the paths and
// sorted, keeping the list stable between refreshes.
func allowRules(root, events string, workspaces []string, aliases map[string]string) []string {
	rules := []string{
		absRule("Read", filepath.Join(root, "**")),
		absRule("Read", filepath.Join(events, "**")),
		"Read(~/**)",
	}
	for _, ws := range workspaces {
		if r := absRule("Read", filepath.Join(ws, "**")); !slices.Contains(rules, r) {
			rules = append(rules, r)
		}
	}
	for _, alias := range slices.Sorted(maps.Keys(aliases)) {
		if r := absRule("Read", filepath.Join(alias, "**")); !slices.Contains(rules, r) {
			rules = append(rules, r)
		}
	}
	return rules
}

// spellings are the paths the Planner may name dir by: dir made absolute,
// dir with its symlinks resolved, and dir under each workspace alias whose
// directory holds it. A deny rule written for one spelling would leave the
// file readable by another an allow rule covers, so each gets one. dir need
// not exist yet: the part that does is resolved and the rest kept, so the
// rules do not change (and restart the Planner) once it is created.
func spellings(dir string, aliases map[string]string) []string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	out := []string{dir}
	if real := resolveExisting(dir); real != dir {
		out = append(out, real)
	}
	for _, alias := range slices.Sorted(maps.Keys(aliases)) {
		target := aliases[alias]
		for _, p := range out[:min(len(out), 2)] {
			var s string
			if p == target {
				s = alias
			} else if rest, ok := strings.CutPrefix(p, target+string(filepath.Separator)); ok {
				s = filepath.Join(alias, rest)
			}
			if s != "" && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// resolveExisting resolves the symlinks of p's longest existing ancestor
// and keeps the components below it.
func resolveExisting(p string) string {
	var rest []string
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(append([]string{real}, rest...)...)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = parent
	}
}

// insideRules carry deny rules rooted at dir (pats relative to it, "" for
// dir itself) to the aliases whose directory lies inside dir. A file
// reached through such an alias is matched only by the alias's spelling,
// which is no spelling of dir, so spellings misses it. An alias gets what
// the pattern still matches below its directory, or all of it when the
// pattern names a tree the directory sits in.
func insideRules(dir string, pats []string, aliases map[string]string) []string {
	bases := spellings(dir, nil)
	var rules []string
	for _, alias := range slices.Sorted(maps.Keys(aliases)) {
		for _, b := range bases {
			rel, ok := strings.CutPrefix(aliases[alias], b+string(filepath.Separator))
			if !ok {
				continue
			}
			for _, p := range pats {
				if tail, ok := patternBelow(p, rel); ok {
					if r := absRule("Read", filepath.Join(alias, tail)); !slices.Contains(rules, r) {
						rules = append(rules, r)
					}
				}
			}
			break
		}
	}
	return rules
}

// patternBelow is what pat, relative to a directory, matches below its
// subdirectory rel: the rest of pat once its leading components matched
// rel's, "**" when pat names rel or one of its ancestors (a denied
// directory covers what is in it), the pattern from a "**" on since that
// spans any depth. ok is false when pat matches nothing below rel.
func patternBelow(pat, rel string) (string, bool) {
	var pc []string
	if pat != "" {
		pc = strings.Split(filepath.ToSlash(pat), "/")
	}
	rc := strings.Split(filepath.ToSlash(rel), "/")
	for i, r := range rc {
		if i == len(pc) {
			return "**", true
		}
		if pc[i] == "**" {
			return filepath.FromSlash(path.Join(pc[i:]...)), true
		}
		if m, err := path.Match(pc[i], r); err != nil || !m {
			return "", false
		}
	}
	if len(pc) == len(rc) {
		return "**", true
	}
	return filepath.FromSlash(path.Join(pc[len(rc):]...)), true
}

// spelledRules deny pats, relative to dir, under every spelling of dir and
// inside every alias that lies within it.
func spelledRules(dir string, pats []string, aliases map[string]string) []string {
	var rules []string
	for _, s := range spellings(dir, aliases) {
		for _, p := range pats {
			rules = append(rules, absRule("Read", filepath.Join(s, p)))
		}
	}
	return append(rules, insideRules(dir, pats, aliases)...)
}

// homeDeny are the home-relative credential paths §15.1 lists, and, with
// the whole home readable (D129), the other places a login keeps secrets:
// the Midway cookie, the keychains and browser profiles under Library, the
// tools' config directories, the shell's history and startup files.
var homeDeny = []string{
	".aws/**", ".ssh/**",
	".naozhi/*.yaml", ".naozhi/**/*.yaml", ".naozhi/*.json", ".naozhi/shims/**", ".naozhi/access-profile-secrets/**",
	".gnupg/**", ".config/**", ".docker/config.json", ".kube/**",
	".netrc", ".git-credentials", ".npmrc",
	".claude.json", ".claude/.credentials.json", ".claude/settings*.json",
	".midway/**", ".ada/**", "Library/**", ".password-store/**",
	".zsh_history", ".bash_history", ".zshenv", ".zshrc", ".zprofile", ".bashrc", ".bash_profile", ".profile",
}

// denyRules keep credentials out of reach even inside an allowed tree: the
// home-relative ones (homeDeny), the data root's secret-bearing files (its
// config-shaped files, shim state with auth tokens, profile secrets, debug
// captures with API keys) and the extra paths, each under every spelling
// (spelledRules). The CLI expands "~" to the one home spelling it was
// given, so the other spellings of home get absolute rules.
func denyRules(home, dataRoot string, extra []string, aliases map[string]string) []string {
	var rules []string
	for _, p := range homeDeny {
		rules = append(rules, "Read(~/"+p+")")
	}
	rules = append(rules,
		"Read(//**/.env)",
		"Read(//**/.env.*)",
		"Read(//**/*.pem)",
		"Read(//**/*.key)",
		"Read(//**/id_rsa*)",
		"Read(//**/id_ed25519*)",
	)
	if home != "" {
		pats := make([]string, len(homeDeny))
		for i, p := range homeDeny {
			pats[i] = filepath.FromSlash(p)
		}
		// The first spelling is home as given, which "~" already covers.
		for _, s := range spellings(home, aliases)[1:] {
			for _, p := range pats {
				rules = append(rules, absRule("Read", filepath.Join(s, p)))
			}
		}
		rules = append(rules, insideRules(home, pats, aliases)...)
	}
	if dataRoot != "" {
		rules = append(rules, spelledRules(dataRoot, []string{"*.yaml", filepath.Join("**", "*.yaml"), "*.json", filepath.Join("shims", "**"),
			filepath.Join("access-profile-secrets", "**"), filepath.Join("session-settings", "**"), filepath.Join("cli-debug", "**")}, aliases)...)
	}
	for _, p := range extra {
		if p == "" {
			continue
		}
		pats := []string{""}
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			pats = append(pats, "**")
		}
		rules = append(rules, spelledRules(p, pats, aliases)...)
	}
	return rules
}

// denyRules is the Planner's deny rules: the credentials (denyRules),
// under the spellings aliases adds. A session marked as having read web
// pages or mail keeps its mark on its digest; its event log is no longer
// denied, the Planner reading such sources itself (D129).
func (a *Agent) denyRules(aliases map[string]string) []string {
	return denyRules(a.cfg.Home, a.cfg.Layout.Root(), a.cfg.DenyPaths, aliases)
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
// changes its shift when the conversation grew too large or an upgrade
// changed its prompt. With planner.enabled off it only stops a
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
	// A busy Planner changes at a later reconcile, so the handoff request
	// runs at once rather than queued behind a turn; one whose summary
	// failed waits out its retry.
	due := !st.Running && !now.Before(a.shiftRetryAt)
	a.mu.Unlock()
	// The conversation is kept from day to day (D130): a new one starts only
	// on a new prompt or past the size limit.
	switch {
	case shift == "":
		a.markShift(now)
	case prompt != PromptVersion && due:
		// An upgrade changed the system prompt, which the resumed
		// conversation would keep. A fresh conversation was marked above, so
		// this is one started on the old prompt, even when its session ID is
		// unknown (a shim adopted before its first turn).
		a.changeShift(ctx, now, fmt.Sprintf("%s-v%d", shiftDay(now), PromptVersion))
	case prompt != PromptVersion:
	case pending != "":
		a.sendHandoff(pending, nil, opts)
	case due && a.oversized(now, st.SessionID):
		a.changeShift(ctx, now, a.sizeShiftName(now))
	default:
		a.refreshScope(st)
		a.flushHeld(now)
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

// refreshScope keeps the Planner's scope in step with the workspace
// registry and cli.mcp_config: a new one goes into the spawn options at
// once, and a live process started on another is released while idle, to be
// resumed on its conversation with it at the next reconcile.
func (a *Agent) refreshScope(st SessionState) {
	want := a.scope()
	a.mu.Lock()
	a.cur = want
	want.apply(&a.opts)
	stale := a.state.Scope != want.sum()
	shifting := a.shifting
	a.mu.Unlock()
	if !stale || !st.Alive || st.Running || shifting {
		return
	}
	if a.cfg.Sessions.Release(Key) {
		slog.Info("planner: scope changed; restarting on the conversation with the new one",
			"tools", len(want.tools), "allow", len(want.allow), "deny", len(want.deny))
		a.poke()
	}
}

// markScopeLocked records that the process just started runs on a.cur.
func (a *Agent) markScopeLocked() {
	if sum := a.cur.sum(); a.state.Scope != sum {
		a.state.Scope = sum
		a.saveLocked()
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
	a.markScopeLocked()
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

// shiftDay is the day a shift starting at t is named by: a day starts at
// 08:30 local, so before then t is still in the previous one.
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
// and the 任务 index. day names the summary: day and prompt version for a
// change an upgrade forces, or day and "-c<n>" for one by size. A summary
// already stored under that name is reused rather than asked for again. It
// stays pending until sent, so after a failed restart the conversation
// revival starts still gets it. A session that could not write one keeps
// its conversation and is asked again later (deferShift): the summary is
// all the new conversation knows of the old.
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
		text, res := a.askHandoff(ctx)
		if ctx.Err() != nil {
			return // shutting down: the next start changes the shift
		}
		if a.deferShift(now, res) {
			return
		}
		if text != "" {
			// Checked once, before it is stored: a stored summary handed
			// over again after a failed restart is not swept twice.
			text = a.checkHandoff(day, text)
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
	a.handoffFails, a.shiftRetryAt = 0, time.Time{}
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
	a.markScopeLocked()
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

// handoffResult is how asking for the handoff summary went.
type handoffResult int

const (
	// handoffWritten: the reply is the summary.
	handoffWritten handoffResult = iota
	// handoffNothing: nothing was said in the conversation; it changes with
	// no summary.
	handoffNothing
	// handoffUnreachable: the model could not be called (the CLI answers
	// such a turn with the API error as the reply's text; a credential
	// expired overnight was once stored as the summary).
	handoffUnreachable
	// handoffFailed: the turn failed, timed out or said nothing.
	handoffFailed
	// handoffBroken: the conversation is past the model's context; no later
	// turn of it can answer, so it changes with no summary now.
	handoffBroken
)

// deferShift reports whether the change of shift is put off after res, and
// until when it is not tried again (shiftRetryAt): always while the model is
// unreachable, up to handoffTries times in a row for a failed turn.
func (a *Agent) deferShift(now time.Time, res handoffResult) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch res {
	case handoffUnreachable:
		a.shiftRetryAt = now.Add(handoffRetry)
		slog.Warn("planner: model unreachable for the handoff summary; keeping the conversation", "retry_in", handoffRetry)
		return true
	case handoffFailed:
		a.handoffFails++
		if a.handoffFails >= handoffTries {
			slog.Warn("planner: no handoff summary; changing shift without one", "failures", a.handoffFails)
			return false
		}
		wait := handoffRetry * time.Duration(a.handoffFails)
		a.shiftRetryAt = now.Add(wait)
		slog.Warn("planner: no handoff summary; keeping the conversation", "failures", a.handoffFails, "retry_in", wait)
		return true
	case handoffBroken:
		slog.Warn("planner: conversation past the model's context; changing shift without a summary")
	}
	return false
}

// askHandoff asks the outgoing session for its summary and waits up to
// handoffWait; text is the summary when res is handoffWritten.
func (a *Agent) askHandoff(ctx context.Context) (text string, res handoffResult) {
	if a.cfg.Sessions.State(Key).SessionID == "" {
		return "", handoffNothing
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
		return "", handoffFailed
	}
	t := time.NewTimer(handoffWait)
	defer t.Stop()
	select {
	case r := <-ch:
		text := strings.TrimSpace(r.text)
		switch {
		case r.err != nil:
			slog.Warn("planner: handoff turn failed", "err", r.err)
		case text == "":
			slog.Warn("planner: handoff turn answered with no summary")
		case apierr.IsEnvelope(text):
			if kind, _ := apierr.ClassifyError(text); kind == apierr.KindContextLength {
				return "", handoffBroken
			}
			return "", handoffUnreachable
		default:
			return textutil.TruncateRunes(text, handoffMaxRunes), handoffWritten
		}
		return "", handoffFailed
	case <-t.C:
		slog.Warn("planner: handoff timed out")
	case <-ctx.Done():
	}
	// A request that never started would otherwise run first on the new
	// conversation once the restart frees the queue.
	if queued {
		turns.DropPlannerQueue(Key)
	}
	return "", handoffFailed
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
	a.mu.Lock()
	a.userAt = a.cfg.Now()
	a.mu.Unlock()
	return nil
}

// Tell sends the gateway's own message to the Planner: the outcome of an
// action block (a dispatch, the user's answer). It carries no snapshot and
// gets no receipt. The receipts waiting (TellLater, TellAlong) go in the
// same turn, ahead of text; with none, an empty text sends nothing.
// Tells run one at a time (tellMu), so two cannot both send the receipts
// waiting, and drop only what they sent: one held meanwhile stays.
func (a *Agent) Tell(text string) error {
	a.tellMu.Lock()
	defer a.tellMu.Unlock()
	text, sent := a.withHeld(text)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if err := a.tell(SourceReceipt, text); err != nil {
		return err
	}
	a.dropHeld(sent)
	return nil
}

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
	snap, routines := a.health, a.routines
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
	if a.cfg.Fronts != nil {
		in.Fronts = orEmpty(a.cfg.Fronts())
	}
	if routines != nil {
		in.Routines = orEmpty(routines())
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
	ss := a.cfg.Sessions.State(Key)
	st.Alive, st.Running = ss.Alive, ss.Running
	st.Workspaces = a.workspaceNames()
	return st
}

// ShiftName names the current conversation's shift: the day it started and
// when. A web lookup's count is per shift (planner/reader).
func (a *Agent) ShiftName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state.Shift + "@" + a.state.ShiftAt.Format(time.RFC3339)
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

// asideTitleRunes caps an aside chat's title in its note: the page shows it
// on one line.
const asideTitleRunes = 60

// AsideNote is the line an aside chat (单独聊, §15.14 ⑥, D117) leaves in the
// conversation, "💬 <key> 你开了单独聊：<title>": the page reads the key from
// it to open the session (planner_cards.js, planner_layers.js). The title is
// folded to one line; "" when it is empty.
func AsideNote(key, title string) string {
	title = strings.Join(strings.Fields(title), " ")
	if cut := textutil.TruncateRunesNoEllipsis(title, asideTitleRunes); cut != title {
		title = cut + "…"
	}
	if title == "" {
		return ""
	}
	return "💬 " + key + " 你开了单独聊：" + title
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
