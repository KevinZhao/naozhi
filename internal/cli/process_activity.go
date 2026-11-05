package cli

// process_activity.go — Activity (state change) records derived from the
// process's tool events (docs/rfc/planner.md §3.4, §15.1 W1). A classified
// tool call is reported once its tool_result says it succeeded; PR links in
// tool results and the CLI's code_change_published frames are reported as
// PRs; and at each turn end the working directory is scanned for output
// files the turn wrote some other way (a script, a Bash redirect).

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/gitinfo"
	"github.com/naozhi/naozhi/internal/planner/activity"
)

// prLinkGate is what a user frame mentioning a PR contains; every other user
// frame skips the second decode.
const prLinkGate = "/pull/"

// maxPendingCalls bounds the tool calls awaiting their result, and
// maxReportedPRs the PR URLs remembered for dedup.
const (
	maxPendingCalls = 64
	maxReportedPRs  = 256
)

// activityHook is what SetOnActivity installs.
type activityHook struct {
	cwd         string // scanned at turn end
	allowedRoot string // bounds the walk up to rootDir
	fn          func(activity.Activity)

	rootOnce sync.Once
	root     string
}

// rootDir is the directory output paths are relative to: gitinfo.TreeRoot,
// the root a root=session:<key> file-API request resolves to. Resolved on
// first use, on the read loop, so binding the hook does no I/O.
func (h *activityHook) rootDir() string {
	h.rootOnce.Do(func() { h.root = gitinfo.TreeRoot(h.cwd, h.allowedRoot) })
	return h.root
}

// activityTracker is the per-process state behind the hook. readLoop is its
// only user, apart from the scanning flag the turn-end scan clears.
type activityTracker struct {
	// pending are this turn's tool calls whose result is awaited: the
	// classified ones, and the Bash / MCP calls whose output may link a PR.
	pending map[string]pendingCall
	// written are the output paths reported this turn, which the scan skips.
	written map[string]struct{}
	// prs maps a reported PR URL to the state it was reported with.
	prs       map[string]string
	turnStart time.Time
	scanning  atomic.Bool
}

// pendingCall is a tool call awaiting its result: the state changes it makes
// once it succeeds (none for a Bash / MCP call kept only for its links).
type pendingCall struct {
	acts []activity.Activity
}

// SetOnActivity installs fn to receive the activities this process's tool
// calls produce in the absolute working directory cwd, which is also scanned
// for outputs at each turn end. allowedRoot (the dashboard's allowed_root,
// "" for none) bounds the search for the git tree output paths are relative
// to, as the file API's does. Lock-free, so safe at any time; a nil fn or a
// relative cwd clears it. fn runs on the read loop or the scan goroutine and
// must not block.
func (p *Process) SetOnActivity(cwd, allowedRoot string, fn func(activity.Activity)) {
	if fn == nil || !filepath.IsAbs(cwd) {
		p.activityHook.Store(nil)
		return
	}
	p.activityHook.Store(&activityHook{cwd: cwd, allowedRoot: allowedRoot, fn: fn})
}

// activityRoot is the root Write / Edit entries' paths are relative to, ""
// without a hook.
func (p *Process) activityRoot() string {
	if h := p.activityHook.Load(); h != nil {
		return h.rootDir()
	}
	return ""
}

// observeActivity feeds one frame to the tracker. Called from readLoop for
// every frame, before the passthrough paths that return early.
func (p *Process) observeActivity(ev *clievent.Event, now time.Time) {
	h := p.activityHook.Load()
	if h == nil {
		return
	}
	t := &p.acts
	if ev.Type != "result" && t.turnStart.IsZero() {
		t.turnStart = now
	}
	switch ev.Type {
	case "assistant":
		if ev.Message == nil {
			return
		}
		for _, b := range ev.Message.Content {
			if b.Type != "tool_use" || b.ID == "" || len(t.pending) >= maxPendingCalls {
				continue
			}
			acts := classifyToolUse(b, h.rootDir())
			if len(acts) == 0 && b.Name != "Bash" && !strings.HasPrefix(b.Name, "mcp__") {
				continue
			}
			if t.pending == nil {
				t.pending = make(map[string]pendingCall)
			}
			t.pending[b.ID] = pendingCall{acts: acts}
		}
	case "user":
		if ev.Message == nil {
			return
		}
		for _, b := range ev.Message.Content {
			if b.Type != "tool_result" {
				continue
			}
			call, ok := t.pending[b.ToolUseID]
			if !ok {
				continue
			}
			delete(t.pending, b.ToolUseID)
			// A failed call changed nothing, and the links in its error
			// ("a pull request already exists: ...") are not news either.
			if b.IsError {
				continue
			}
			// The first link names the PR a create or merge without one
			// acted on; the rest, and every link of another call, report as
			// PRs (reportActivity drops the one just reported).
			links := ev.ToolResultPRs[b.ToolUseID]
			linked := false
			for _, a := range call.acts {
				if (a.Type == activity.TypePRCreate || a.Type == activity.TypePRMerge) && a.URL == "" && len(links) > 0 && !linked {
					a.URL, linked = links[0], true
				}
				p.reportActivity(h, a)
			}
			for _, u := range links {
				p.reportActivity(h, activity.Activity{Type: activity.TypePR, Summary: u, URL: u})
			}
		}
	case "system":
		if c := ev.CodeChange; c != nil && c.Provider == "github" {
			a := activity.Activity{Type: activity.TypePR, Summary: c.URL, URL: c.URL}
			switch c.Action {
			case "created":
				a.State = activity.StateOpened
			case "merged":
				a.State = activity.StateMerged
			}
			p.reportActivity(h, a)
		}
	case "result":
		start, written := t.turnStart, t.written
		t.turnStart, t.written = time.Time{}, nil
		clear(t.pending)
		if !start.IsZero() && h.cwd != "" && t.scanning.CompareAndSwap(false, true) {
			go p.scanOutputs(h, start, written)
		}
	}
}

// reportActivity hands a to the hook, once per output path per turn and
// once per PR URL and state.
func (p *Process) reportActivity(h *activityHook, a activity.Activity) {
	t := &p.acts
	switch {
	case a.Type == activity.TypeOutput:
		if _, dup := t.written[a.Path]; dup {
			return
		}
		if t.written == nil {
			t.written = make(map[string]struct{})
		}
		t.written[a.Path] = struct{}{}
	case a.URL != "":
		if state, seen := t.prs[a.URL]; seen && (a.State == "" || a.State == state) {
			return
		}
		if t.prs == nil || len(t.prs) >= maxReportedPRs {
			t.prs = make(map[string]string)
		}
		t.prs[a.URL] = a.State
	}
	h.fn(a)
}

// scanOutputs reports the output files under h.cwd the turn that began at
// start wrote without a Write / Edit call. Off the read loop: a turn end must
// not wait on a directory walk.
func (p *Process) scanOutputs(h *activityHook, start time.Time, written map[string]struct{}) {
	defer p.acts.scanning.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), activity.ScanTimeout)
	defer cancel()
	// Whole seconds: on a filesystem with coarse mtimes a file written in
	// the turn's first second would otherwise read as older than the turn.
	for _, abs := range activity.Scan(ctx, h.cwd, start.Truncate(time.Second)) {
		rel, ok := activity.RelPath(h.rootDir(), abs)
		if !ok {
			continue
		}
		if _, dup := written[rel]; dup {
			continue
		}
		h.fn(activity.Activity{Type: activity.TypeOutput, Summary: rel, Path: rel})
	}
}

// classifyToolUse classifies one tool_use block, making an output's path
// relative to root; an output outside root is not one (it cannot be
// previewed, and is usually CLI state such as a plan file).
func classifyToolUse(b clievent.ContentBlock, root string) []activity.Activity {
	acts, _ := activity.Classify(b.Name, b.Input)
	out := acts[:0]
	for _, a := range acts {
		if a.Type == activity.TypeOutput {
			rel, inRoot := activity.RelPath(root, a.Path)
			if !inRoot {
				continue
			}
			a.Path, a.Summary = rel, rel
		}
		out = append(out, a)
	}
	return out
}

// tagToolEntry sets a tool_use entry's Path (any Write / Edit target under
// root) and Activity, the types the call is classified as. The tag is what
// the call attempts: it is set before the call runs, so a call that fails
// keeps it, and the PRs its result links and the turn-end scan's outputs
// have no entry of their own. The journal (planner/activity.Recorder) is the
// complete record.
func tagToolEntry(entry *clievent.EventEntry, b clievent.ContentBlock, root string) {
	acts, target := activity.Classify(b.Name, b.Input)
	if target != "" {
		if rel, inRoot := activity.RelPath(root, target); inRoot {
			entry.Path = rel
		}
	}
	var types []string
	for _, a := range acts {
		if a.Type != activity.TypeOutput || entry.Path != "" {
			types = append(types, string(a.Type))
		}
	}
	entry.Activity = strings.Join(types, ",")
	if strings.HasSuffix(b.Name, "email_read") {
		var in struct {
			MarkAs json.RawMessage `json:"markAs"`
		}
		entry.MarkAs = json.Unmarshal(b.Input, &in) == nil && len(in.MarkAs) > 0 && string(in.MarkAs) != "null"
	}
}

// parseToolResultPRs returns the PR URLs each tool_result block of a user
// frame mentions, keyed by tool_use id; nil when none does.
func parseToolResultPRs(line string) map[string][]string {
	var f wireToolResultFrame
	if json.Unmarshal(stringToBytesUnsafe(line), &f) != nil {
		return nil
	}
	var out map[string][]string
	for _, b := range f.Message.Content {
		if b.Type != "tool_result" || b.ToolUseID == "" {
			continue
		}
		links := activity.PRLinks(toolResultText(b.Content))
		if len(links) == 0 {
			continue
		}
		if out == nil {
			out = make(map[string][]string)
		}
		out[b.ToolUseID] = links
	}
	return out
}

// toolResultText is a tool_result's content as text: a plain string, or the
// text parts of a content array joined.
func toolResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []wireToolResultPart
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if p.Type == "text" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}
