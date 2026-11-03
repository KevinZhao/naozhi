package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

type fakeGateway struct {
	spawned  []string
	spawns   []Spawn
	spawnErr error
	notes    []string
	told     []string
	sideDir  string
}

func newRunner(t *testing.T) (*Runner, *fakeGateway, *notify.Notifier, *sidecar.Store, string) {
	t.Helper()
	g := &fakeGateway{}
	inbox := notify.New(notify.Config{System: func(string, string) error { return nil }})
	reg := workspace.Load("", "node1")
	// The registry stores the resolved path (/private/var on macOS).
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	if _, ok := reg.Ensure(dir); !ok {
		t.Fatal("workspace not registered")
	}
	g.sideDir = t.TempDir()
	side := sidecar.Open(g.sideDir, nil)
	r := New(Config{
		Planner:    sessionkey.PlannerMainKey,
		Inbox:      inbox,
		Workspaces: reg,
		Sessions:   side,
		Spawn: func(_ context.Context, key string, s Spawn) error {
			if g.spawnErr != nil {
				return g.spawnErr
			}
			g.spawned, g.spawns = append(g.spawned, key), append(g.spawns, s)
			return nil
		},
		Note: func(text string) { g.notes = append(g.notes, text) },
		Tell: func(text string) error { g.told = append(g.told, text); return nil },
	})
	return r, g, inbox, side, dir
}

func openDecision(t *testing.T, inbox *notify.Notifier, handler string) notify.Item {
	t.Helper()
	open, _ := inbox.List(0)
	for _, it := range open {
		if it.Decision != nil && it.Decision.Handler == handler {
			return it
		}
	}
	t.Fatalf("no open %s card in %+v", handler, open)
	return notify.Item{}
}

// A dispatch block opens a card; approving it starts one task: session in
// the workspace's directory, records its lineage and tells the Planner; the
// first answer wins.
func TestDispatchApproveSpawns(t *testing.T) {
	r, g, inbox, side, dir := newRunner(t)
	name := dir[strings.LastIndex(dir, "/")+1:]
	r.Result("dashboard:direct:x:general", block(`{"type":"ask","question":"not the Planner","options":[]}`))
	if len(r.ch) != 0 {
		t.Fatal("another session's reply was queued")
	}
	r.Handle(block(`{"type":"dispatch","workspace":"` + name + `","title":"iso 写 hello","model":"","message":"在工作区里写一个 hello.md"}`))
	if len(g.spawned) != 0 {
		t.Fatal("spawned before approval")
	}
	it := openDecision(t, inbox, HandlerDispatch)
	if it.Kind != notify.KindDecision || it.Unit != sessionkey.PlannerMainKey || !strings.Contains(it.Why, "hello.md") {
		t.Errorf("card = %+v", it)
	}
	if len(g.notes) != 1 || !strings.Contains(g.notes[0], "待你确认") {
		t.Errorf("notes = %q", g.notes)
	}

	if err := inbox.Decide(it.ID, notify.ActionApprove); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if len(g.spawned) != 1 || !sessionkey.IsTaskKey(g.spawned[0]) {
		t.Fatalf("spawned = %q", g.spawned)
	}
	key := g.spawned[0]
	if s := g.spawns[0]; s.Dir != dir || s.Title != "iso 写 hello" || s.Message != "在工作区里写一个 hello.md" || s.Model != "" {
		t.Errorf("spawn = %+v", s)
	}
	m, ok := side.Get(key)
	if !ok || m.Purpose != sidecar.PurposeTask || m.Parent != sessionkey.PlannerMainKey || m.Title != "iso 写 hello" || !m.DispatchedByPlanner() || m.Created.IsZero() {
		t.Errorf("sidecar = %+v, %v", m, ok)
	}
	if len(g.told) != 1 || !strings.Contains(g.told[0], "已派出") || !strings.Contains(g.told[0], key) {
		t.Errorf("told = %q", g.told)
	}
	if err := inbox.Decide(it.ID, notify.ActionApprove); !errors.Is(err, notify.ErrClosed) {
		t.Errorf("second answer: %v, want ErrClosed", err)
	}
	if len(g.spawned) != 1 {
		t.Errorf("second answer spawned again: %q", g.spawned)
	}
}

func TestDispatchRejectAndFailure(t *testing.T) {
	r, g, inbox, _, dir := newRunner(t)
	name := dir[strings.LastIndex(dir, "/")+1:]
	d := block(`{"type":"dispatch","workspace":"` + name + `","title":"t","model":"","message":"m"}`)

	r.Handle(d)
	if err := inbox.Decide(openDecision(t, inbox, HandlerDispatch).ID, notify.ActionReject); err != nil {
		t.Fatal(err)
	}
	if len(g.spawned) != 0 || len(g.told) != 1 || !strings.Contains(g.told[0], "驳回") {
		t.Errorf("reject: spawned %q told %q", g.spawned, g.told)
	}

	// A failed spawn leaves the card open and no sidecar behind.
	r.Handle(d)
	it := openDecision(t, inbox, HandlerDispatch)
	g.spawnErr = errors.New("max_procs")
	if err := inbox.Decide(it.ID, notify.ActionApprove); err == nil {
		t.Fatal("approve with a failing spawn succeeded")
	}
	if open := openDecision(t, inbox, HandlerDispatch); open.ID != it.ID {
		t.Errorf("card closed after a failed spawn")
	}
	if left, _ := os.ReadDir(g.sideDir); len(left) != 0 {
		t.Errorf("%d sidecars left after a failed spawn", len(left))
	}

	// An unknown workspace opens no card.
	r.Handle(block(`{"type":"dispatch","workspace":"nowhere","title":"t","model":"","message":"m"}`))
	if last := g.notes[len(g.notes)-1]; !strings.Contains(last, "找不到工作区") {
		t.Errorf("note = %q", last)
	}
}

// An ask with options is answered from its card, and the answer goes back to
// the Planner; a refused block only shows in the conversation.
func TestAskAndRefused(t *testing.T) {
	r, g, inbox, _, _ := newRunner(t)
	r.Handle("看这里：\n" + block(`{"type":"ask","question":"今天发版吗？","options":["发","不发"]}`) + "\n" + block(`{"type":"halt"}`))
	it := openDecision(t, inbox, HandlerAsk)
	if it.Kind != notify.KindQuestion || !slices.Equal(it.Decision.Actions, []notify.Action{"发", "不发"}) {
		t.Errorf("ask card = %+v", it)
	}
	if len(g.notes) != 1 || !strings.Contains(g.notes[0], "格式不对") || !strings.Contains(g.notes[0], `{"type":"halt"}`) {
		t.Errorf("notes = %q", g.notes)
	}
	if err := inbox.Decide(it.ID, "不发"); err != nil {
		t.Fatal(err)
	}
	if len(g.told) != 1 || !strings.Contains(g.told[0], "今天发版吗？") || !strings.HasSuffix(g.told[0], "不发") {
		t.Errorf("told = %q", g.told)
	}
}
