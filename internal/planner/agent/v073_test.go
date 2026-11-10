package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/planner/workspace"
)

// writeTranscript writes the Planner conversation sid's transcript under
// claudeDir with its last call sent with tokens of context, modified at mod.
func writeTranscript(t *testing.T, a *Agent, claudeDir, sid string, tokens int64, mod time.Time) string {
	t.Helper()
	path := claudefs.SessionJSONL(claudeDir, a.cfg.Layout.PlannerWorkDir(), sid)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"assistant","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":%d,"cache_creation_input_tokens":0}}}`+"\n", tokens-10)
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSizeShift: past 250k tokens of context the shift changes once the
// Planner has been idle 5 minutes, named <day>-c<n>, and not again within
// 2 hours of the shift's start.
func TestSizeShift(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, loc)
	sess := &fakeSessions{st: SessionState{Exists: true, Alive: true, SessionID: "s1"}}
	a, lay := newTestAgent(t, sess, &now)
	claudeDir := t.TempDir()
	a.cfg.ClaudeDir = claudeDir
	turns := &fakeTurns{reply: "## 进行中\n- 线 A：在跑"}
	a.turns = turns
	a.SetTaskIndex(func() []TaskLine {
		return []TaskLine{{ID: "task:abc", State: "进行中", Title: "修登录", Conclusion: "已复现", Digest: "/d/abc.md"}}
	})
	ctx := context.Background()
	a.reconcile(ctx) // marks today's shift
	a.mu.Lock()
	a.state.ShiftAt = now.Add(-3 * time.Hour)
	a.mu.Unlock()

	// Small context: nothing.
	writeTranscript(t, a, claudeDir, "s1", 120_000, now.Add(-10*time.Minute))
	a.reconcile(ctx)
	if sess.recreated != 0 {
		t.Fatalf("small context: recreated %d", sess.recreated)
	}
	// Large but touched 2 minutes ago: not idle yet.
	writeTranscript(t, a, claudeDir, "s1", 260_000, now.Add(-2*time.Minute))
	a.reconcile(ctx)
	if sess.recreated != 0 {
		t.Fatalf("not idle: recreated %d", sess.recreated)
	}
	// Running: waits.
	writeTranscript(t, a, claudeDir, "s1", 260_000, now.Add(-6*time.Minute))
	sess.mu.Lock()
	sess.st.Running = true
	sess.mu.Unlock()
	a.reconcile(ctx)
	if sess.recreated != 0 {
		t.Fatalf("running: recreated %d", sess.recreated)
	}
	sess.mu.Lock()
	sess.st.Running = false
	sess.mu.Unlock()
	a.reconcile(ctx)
	if sess.recreated != 1 {
		t.Fatalf("idle and large: recreated %d, want 1", sess.recreated)
	}
	if _, err := os.Stat(filepath.Join(lay.PlannerHandoffRoot(), "2026-11-10-c1.md")); err != nil {
		t.Fatalf("handoff 2026-11-10-c1.md: %v", err)
	}
	last := turns.sent[len(turns.sent)-1]
	for _, want := range []string{`<naozhi-handoff day="2026-11-10-c1">`, "<naozhi-tasks", "进行中 · 修登录 · `task:abc` · 结论「已复现」 · 摘要 /d/abc.md"} {
		if !strings.Contains(last, want) {
			t.Errorf("first message lacks %q:\n%s", want, last)
		}
	}

	// The new conversation grows past the limit at once: it waits 2 hours
	// from its shift's start.
	sess.mu.Lock()
	sess.st.SessionID = "s2"
	sess.mu.Unlock()
	now = now.Add(time.Hour)
	writeTranscript(t, a, claudeDir, "s2", 300_000, now.Add(-10*time.Minute))
	a.reconcile(ctx)
	if sess.recreated != 1 {
		t.Fatalf("within 2 hours: recreated %d", sess.recreated)
	}
	now = now.Add(time.Hour + time.Minute)
	writeTranscript(t, a, claudeDir, "s2", 300_000, now.Add(-10*time.Minute))
	a.reconcile(ctx)
	if sess.recreated != 2 {
		t.Fatalf("past 2 hours: recreated %d, want 2", sess.recreated)
	}
	if _, err := os.Stat(filepath.Join(lay.PlannerHandoffRoot(), "2026-11-10-c2.md")); err != nil {
		t.Fatalf("second change by size is c2: %v", err)
	}
}

// TestHandoffPrompt: the summary is asked by thread, within 2,000 字.
func TestHandoffPrompt(t *testing.T) {
	for _, want := range []string{"2000 字", "每条不超过 200 字", "## 进行中", "## 答应用户的事", "## 用户新说的偏好与决定", "## 待跟进"} {
		if !strings.Contains(handoffPrompt, want) {
			t.Errorf("handoff prompt lacks %q", want)
		}
	}
	if got := handoffMessage("d", "s", ""); strings.Contains(got, "naozhi-tasks") {
		t.Errorf("no index: %q", got)
	}
	lines := make([]TaskLine, taskIndexMax+3)
	for i := range lines {
		lines[i] = TaskLine{ID: fmt.Sprintf("task:%d", i), State: "已完成", Title: "t"}
	}
	idx := renderTaskIndex(lines, "09:00")
	if n := strings.Count(idx, "\n- "); n != taskIndexMax+1 || !strings.Contains(idx, "另外 3 个") {
		t.Errorf("index of %d: %d lines\n%s", len(lines), n, idx)
	}
}

// TestReadScopeRefresh: a workspace registered while the Planner runs puts
// its rule into the spawn options at once and has the idle process released,
// to be resumed with it; a busy one waits.
func TestReadScopeRefresh(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.Local)
	sess := &fakeSessions{st: SessionState{Exists: true, Alive: true, SessionID: "s1"}}
	a, _ := newTestAgent(t, sess, &now)
	reg := workspace.Load("", "local")
	a.cfg.Workspaces = reg
	a.turns = &fakeTurns{}
	ctx := context.Background()
	a.reconcile(ctx)
	if !sess.st.Alive {
		t.Fatal("released with nothing changed")
	}
	if _, ok := reg.Ensure(t.TempDir()); !ok {
		t.Fatal("workspace not registered")
	}
	rule := absRule("Read", filepath.Join(reg.LocalPaths()[0], "**"))

	sess.mu.Lock()
	sess.st.Running = true
	sess.mu.Unlock()
	a.reconcile(ctx)
	if !slices.Contains(a.Opts().PermissionsAllow, rule) {
		t.Fatalf("opts lack %s", rule)
	}
	if !sess.st.Alive {
		t.Fatal("released while a turn runs")
	}
	sess.mu.Lock()
	sess.st.Running = false
	sess.mu.Unlock()
	a.reconcile(ctx)
	if sess.st.Alive {
		t.Fatal("idle Planner on old rules not released")
	}
	// The next reconcile resumes it on the new rules, and leaves it be.
	a.reconcile(ctx)
	if !sess.st.Alive || sess.recreated != 0 {
		t.Fatalf("not resumed: alive %v recreated %d", sess.st.Alive, sess.recreated)
	}
	a.reconcile(ctx)
	if !sess.st.Alive {
		t.Fatal("released again after resuming on the new rules")
	}
}

// TestRemember: a memory is written with frontmatter and indexed, replaced
// under the same name, taken back with its index line, and one the gateway
// did not write is neither overwritten nor taken back.
func TestRemember(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	a, _ := newTestAgent(t, &fakeSessions{}, &now)
	claudeDir := t.TempDir()
	a.cfg.ClaudeDir = claudeDir
	reg := workspace.Load("", "local")
	w, ok := reg.Ensure(t.TempDir())
	if !ok {
		t.Fatal("workspace not registered")
	}
	wsDir := reg.LocalPaths()[0]
	a.cfg.Workspaces = reg
	mem := claudefs.MemoryDir(claudefs.ProjectDir(claudeDir, wsDir))
	if err := os.MkdirAll(mem, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mem, "MEMORY.md"), []byte("- [旧](old.md) — 旧的\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := a.Remember(w.ID, "frankfurt-az", "法兰克福用 euc1-az2\n别用 az1")
	if err != nil || path != filepath.Join(mem, "frankfurt-az.md") {
		t.Fatalf("Remember = %q, %v", path, err)
	}
	data, _ := os.ReadFile(path)
	for _, want := range []string{"---\nname: frankfurt-az\n", `description: "法兰克福用 euc1-az2"`, "source: naozhi-planner", "remembered_at: 2026-11-10T12:00:00Z", "\n---\n\n法兰克福用 euc1-az2\n别用 az1\n"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("memory file lacks %q:\n%s", want, data)
		}
	}
	if _, err := a.Remember(w.Name, "frankfurt-az", "改成 az3"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	idx, _ := os.ReadFile(filepath.Join(mem, "MEMORY.md"))
	if string(idx) != "- [旧](old.md) — 旧的\n- [frankfurt-az](frankfurt-az.md) — 改成 az3\n" {
		t.Fatalf("index = %q", idx)
	}

	if err := os.WriteFile(filepath.Join(mem, "old.md"), []byte("---\nname: old\n---\n旧\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Remember(w.ID, "old", "x"); err == nil {
		t.Fatal("overwrote a memory the gateway did not write")
	}
	if err := a.Forget(w.ID, "old"); !errors.Is(err, ErrNotPlanners) {
		t.Fatalf("Forget(old) = %v", err)
	}

	if err := a.Forget(w.ID, "frankfurt-az"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file left: %v", err)
	}
	idx, _ = os.ReadFile(filepath.Join(mem, "MEMORY.md"))
	if string(idx) != "- [旧](old.md) — 旧的\n" {
		t.Fatalf("index after Forget = %q", idx)
	}
	if err := a.Forget(w.ID, "frankfurt-az"); !errors.Is(err, ErrNoMemory) {
		t.Fatalf("second Forget = %v", err)
	}

	// global is the Planner's own project; a bad name or workspace refused.
	gp, err := a.Remember("global", "style", "回复先结论")
	if want := filepath.Join(claudefs.MemoryDir(claudefs.ProjectDir(claudeDir, a.cfg.Layout.PlannerWorkDir())), "style.md"); err != nil || gp != want {
		t.Fatalf("global = %q, %v; want %q", gp, err, want)
	}
	if _, err := a.Remember(w.ID, "../x", "t"); !errors.Is(err, ErrMemoryName) {
		t.Fatalf("bad name: %v", err)
	}
	if _, err := a.Remember("nope", "x", "t"); err == nil {
		t.Fatal("unknown workspace accepted")
	}
}
