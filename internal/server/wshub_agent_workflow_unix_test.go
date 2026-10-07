//go:build unix

package server

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/testhelper"
)

const (
	wfAgentSID  = "04a8fc10-6fa5-4b8e-82ba-621974425917"
	wfAgentRun  = "wf_2997921d-435"
	wfAgentID   = "a2093755b9a9ce8c0"
	wfAgentPrev = "a0ba344a06862740f"
	wfAgentKey  = "dashboard:direct:wf-drill:general"
)

// wfAgentRig is a session with no process whose board lists wfAgentID (and
// wfAgentPrev as its earlier attempt) in a run dir under a real projects
// root, which holds the probe's transcript for both.
type wfAgentRig struct {
	hub    *Hub
	src    *wfSource
	runDir string
	wf     *workflow.Workflow
}

func newWFAgentRig(t *testing.T) *wfAgentRig {
	t.Helper()
	hub, router := newTestHub(t, "")
	t.Cleanup(hub.Shutdown)
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, ws := filepath.Join(tmp, "projects"), filepath.Join(tmp, "ws")
	r := &wfAgentRig{hub: hub, runDir: claudefs.WorkflowRunDir(claudefs.SubagentsDir(filepath.Join(root, claudefs.ProjectSlug(ws)), wfAgentSID), wfAgentRun)}
	for _, d := range []string{ws, r.runDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join("..", "subagent", "testdata", "agent-"+wfAgentID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	r.write(t, wfAgentID, string(data))
	r.write(t, wfAgentPrev, strings.ReplaceAll(string(data), wfAgentID, wfAgentPrev))
	sess := router.InjectSession(wfAgentKey, nil)
	r.src = &wfSource{sess: sess, set: &workflow.Set{}}
	sess.BindWorkflowsOnDiskForTest(r.src, root, ws)
	r.wf = &workflow.Workflow{
		TaskID: "w1", Status: workflow.StatusRunning, RunID: wfAgentRun, SessionID: wfAgentSID, Source: workflow.SourceStream,
		Agents: []workflow.Agent{{Index: 1, Label: "A", State: workflow.AgentRunning, AgentID: wfAgentID, PrevAgentIDs: []string{wfAgentPrev}}},
	}
	r.publish()
	testhelper.Eventually(t, func() bool {
		p := sess.WorkflowBoard().Published()
		return p != nil && len(p.Workflows) == 1 && p.Workflows[0].RunDir != ""
	}, 5*time.Second, "run dir never resolved")
	return r
}

func (r *wfAgentRig) path(id string) string { return claudefs.SubagentJSONL(r.runDir, id) }

func (r *wfAgentRig) write(t *testing.T, id, body string) {
	t.Helper()
	if err := os.WriteFile(r.path(id), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (r *wfAgentRig) publish() {
	w := *r.wf
	w.Agents = append([]workflow.Agent(nil), r.wf.Agents...)
	r.src.Publish(&w)
}

func (r *wfAgentRig) subscribe(t *testing.T, id string) <-chan node.ServerMsg {
	t.Helper()
	c, out := newCapturedClient(t, r.hub)
	r.hub.handleAgentSubscribe(c, node.ClientMsg{Type: "agent_subscribe", Key: wfAgentKey, TaskID: id})
	return out
}

// next returns the next frame of type typ on out, failing after 3s.
func next(t *testing.T, out <-chan node.ServerMsg, typ string) node.ServerMsg {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m := <-out:
			if m.Type == typ {
				return m
			}
		case <-deadline:
			t.Fatalf("no %s frame", typ)
			return node.ServerMsg{}
		}
	}
}

// TestAgentSubscribe_WorkflowAgent is §8.3 on a session with no linker: the
// board's agent is tailed through its opener, the frame-stripped task
// first; when the board marks the row done the tailer sends agent_done.
func TestAgentSubscribe_WorkflowAgent(t *testing.T) {
	t.Parallel()
	r := newWFAgentRig(t)
	out := r.subscribe(t, wfAgentID)
	if ev := next(t, out, "agent_event"); ev.Event == nil || ev.Event.Detail != "Reply with just the number 2+2" || ev.TaskID != wfAgentID {
		t.Fatalf("first agent_event %+v", ev)
	}
	if ev := next(t, out, "agent_event"); ev.Event == nil || ev.Event.Detail != "4" {
		t.Fatalf("second agent_event %+v", ev)
	}
	r.wf.Agents[0].State = workflow.AgentFailed
	r.publish()
	if done := next(t, out, "agent_done"); done.Status != "error" {
		t.Errorf("agent_done status %q, want error", done.Status)
	}
}

// TestAgentSubscribe_WorkflowAgentEarlierAttempt: an earlier attempt's id
// is tailed too and is over at once.
func TestAgentSubscribe_WorkflowAgentEarlierAttempt(t *testing.T) {
	t.Parallel()
	r := newWFAgentRig(t)
	out := r.subscribe(t, wfAgentPrev)
	next(t, out, "agent_event")
	if done := next(t, out, "agent_done"); done.Status != "completed" {
		t.Errorf("agent_done status %q, want completed", done.Status)
	}
}

// TestAgentSubscribe_WorkflowAgentRejected: a transcript not written yet is
// pending; another agent's file and a FIFO are refused, the FIFO at once.
func TestAgentSubscribe_WorkflowAgentRejected(t *testing.T) {
	t.Parallel()
	r := newWFAgentRig(t)
	reject := func(name string) string {
		t.Helper()
		done := make(chan string, 1)
		go func() { done <- next(t, r.subscribe(t, wfAgentID), "agent_subscribe_rejected").Reason }()
		select {
		case reason := <-done:
			return reason
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: agent_subscribe blocked", name)
			return ""
		}
	}
	data, _ := os.ReadFile(r.path(wfAgentPrev))
	r.write(t, wfAgentID, string(data))
	if got := reject("another agent's file"); got != "tombstone" {
		t.Errorf("another agent's file: %q, want tombstone", got)
	}
	if err := os.Remove(r.path(wfAgentID)); err != nil {
		t.Fatal(err)
	}
	if got := reject("missing"); got != "pending" {
		t.Errorf("missing: %q, want pending", got)
	}
	if err := syscall.Mkfifo(r.path(wfAgentID), 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	t.Cleanup(func() {
		if w, err := os.OpenFile(r.path(wfAgentID), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
	})
	if got := reject("FIFO"); got != "tombstone" {
		t.Errorf("FIFO: %q, want tombstone", got)
	}
}

// TestAgentSubscribe_WorkflowAgentSharedTailer: a second subscriber gets
// the tailer the first started, and the fd its first-line check opened is
// closed again, so the transcript stays open once (the tailer's).
func TestAgentSubscribe_WorkflowAgentSharedTailer(t *testing.T) {
	t.Parallel()
	r := newWFAgentRig(t)
	next(t, r.subscribe(t, wfAgentID), "agent_event")
	if n := fdsOnFile(t, r.path(wfAgentID)); n != 1 {
		t.Fatalf("%d fds on the transcript after one subscriber, want the tailer's 1", n)
	}
	first := r.hub.tailers.lookup(tailerKey{wfAgentKey, wfAgentID})
	for range 3 {
		next(t, r.subscribe(t, wfAgentID), "agent_event")
	}
	if n := fdsOnFile(t, r.path(wfAgentID)); n != 1 {
		t.Errorf("%d fds on the transcript after four subscribers, want 1", n)
	}
	if got := r.hub.tailers.lookup(tailerKey{wfAgentKey, wfAgentID}); got != first || r.hub.tailers.count.Load() != 1 {
		t.Errorf("tailer %p (was %p), %d tailers; want the one shared", got, first, r.hub.tailers.count.Load())
	}
}

// fdsOnFile counts this process's fds open on path's file, by fstat of
// each fd /dev/fd lists.
func fdsOnFile(t *testing.T, path string) int {
	t.Helper()
	var want syscall.Stat_t
	if err := syscall.Stat(path, &want); err != nil {
		t.Fatal(err)
	}
	d, err := os.Open("/dev/fd")
	if err != nil {
		t.Skipf("/dev/fd unavailable: %v", err)
	}
	names, err := d.Readdirnames(-1)
	d.Close()
	if err != nil {
		t.Skipf("/dev/fd unreadable: %v", err)
	}
	n := 0
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		var st syscall.Stat_t
		if err == nil && syscall.Fstat(fd, &st) == nil && st.Dev == want.Dev && st.Ino == want.Ino {
			n++
		}
	}
	return n
}
