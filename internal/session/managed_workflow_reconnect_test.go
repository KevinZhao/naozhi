package session

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/shim"
	"github.com/naozhi/naozhi/internal/testhelper"
)

const (
	probeTask      = "w113pvmto"
	probeStartedAt = int64(1_791_170_000_000)
)

// shimScript is what the fake shim plays on one connection: a replay
// starting at firstSeq, live stdout lines, then the CLI's exit when exit is
// set and a close when hangUp is. shimPID is what its hello reports:
// os.Getpid() is a shim that outlives the socket, FakeShimPID one that is gone.
type shimScript struct {
	firstSeq     int64
	replay, live []string
	exit, hangUp bool
	shimPID      int
}

// workflowShim restores key from entry into a router whose fake shim plays
// scripts, one per connection, and returns the router and the session.
func workflowShim(t *testing.T, entry *storeEntry, scripts ...shimScript) (*Router, *ManagedSession) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shim discovery needs unix PID liveness and unix sockets")
	}
	dir := shortTempDir(t)
	mgr, err := shim.NewManager(shim.ManagerConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	w := cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")
	w.ShimManager = mgr
	r := NewRouter(RouterConfig{Wrapper: w, ClaudeDir: filepath.Join(dir, "claude"), HistoryLoader: &racingHistoryLoader{}})
	t.Cleanup(r.Shutdown)
	entry.Workspace, entry.Backend = filepath.Join(dir, "ws"), "claude"
	r.ss.Update(func(tx sessTx) { r.restoreSessionFromEntry(tx, entry.Key, entry) })
	sess := r.SessionFor(entry.Key)

	t.Setenv("XDG_RUNTIME_DIR", dir)
	socket := shim.SocketPath(shim.KeyHash(sess.key))
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for _, sc := range scripts {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
			go playShimScript(conn, sc)
		}
	}()
	writeShimStateFor(t, dir, shim.State{
		ShimPID: os.Getpid(), Socket: socket, Key: sess.key, Backend: "claude", SessionID: resolveStoredSID,
		CLIArgs:      driftArgsFor(r).driftCompareArgs(w, "claude", sess.key, sess, &shim.SpawnOverlay{}),
		SpawnOverlay: &shim.SpawnOverlay{},
	})
	return r, sess
}

func playShimScript(conn net.Conn, sc shimScript) {
	rd := bufio.NewReader(conn)
	if _, err := rd.ReadBytes('\n'); err != nil { // attach
		return
	}
	frames := []shim.ServerMsg{{Type: "hello", ProtocolVersion: shim.ProtocolVersion, ShimPID: sc.shimPID}}
	for i, line := range sc.replay {
		frames = append(frames, shim.ServerMsg{Type: "replay", Seq: sc.firstSeq + int64(i), Line: line})
	}
	frames = append(frames, shim.ServerMsg{Type: "replay_done", Count: len(sc.replay)})
	for i, line := range sc.live {
		frames = append(frames, shim.ServerMsg{Type: "stdout", Seq: sc.firstSeq + int64(len(sc.replay)+i), Line: line})
	}
	if sc.exit {
		code := 1
		frames = append(frames, shim.ServerMsg{Type: "cli_exited", Code: &code})
	}
	for i := range frames {
		data, _ := frames[i].MarshalLine()
		if _, err := conn.Write(data); err != nil {
			return
		}
	}
	if sc.hangUp {
		conn.Close()
		return
	}
	// A shim exits on shutdown; anything else needs no answer.
	for {
		line, err := rd.ReadString('\n')
		if err != nil || strings.Contains(line, `"type":"shutdown"`) {
			conn.Close()
			return
		}
	}
}

// probeRef is the Ref an earlier naozhi saved for the probe's workflow.
func probeRef() []workflow.Ref {
	return []workflow.Ref{{
		TaskID: probeTask, RunID: wfRun, Name: "probe", SessionID: wfSID, Status: workflow.StatusRunning,
		StartedAt: probeStartedAt, LastObservedAt: wfT0, Counts: workflow.Counts{Total: 3},
	}}
}

// probeLines returns the 3-agent probe capture; probeLines(t)[n-1] is line n.
func probeLines(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "cli", "workflow", "testdata", "probe-3agent.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 20 {
		t.Fatalf("probe capture has %d lines, want 20", len(lines))
	}
	return lines
}

// boardEntry is the session board's published entry for id, nil if none.
func boardEntry(s *ManagedSession, id string) *workflow.Workflow {
	for _, w := range s.WorkflowBoard().Published().Workflows {
		if w.TaskID == id {
			return w
		}
	}
	return nil
}

// TestReconnectShims_WorkflowSurvivesRestartAndSocketLoss is PR-8's
// acceptance on a shim that outlived naozhi with its ring wrapped: the
// restored Ref keeps the real name, start and run id the replay no longer
// holds; losing the socket with the shim alive leaves the run running as
// snapshot_stale; the reattached CLI brings it back live, then completed.
func TestReconnectShims_WorkflowSurvivesRestartAndSocketLoss(t *testing.T) {
	lines := probeLines(t)
	wrapped := lines[5:13] // probe lines 6..13: no task_started, no launch
	r, sess := workflowShim(t, &storeEntry{Key: "feishu:direct:alice:general", SessionID: resolveStoredSID, Workflows: probeRef()},
		shimScript{firstSeq: 9001, replay: wrapped, hangUp: true, shimPID: os.Getpid()},
		shimScript{firstSeq: 9001, replay: wrapped, live: lines[14:16], shimPID: os.Getpid()},
	)
	b := sess.WorkflowBoard()
	epoch := b.Published().Epoch

	r.ReconnectShimsCtx(context.Background())
	testhelper.Eventually(t, func() bool {
		w := boardEntry(sess, probeTask)
		return w != nil && w.Degraded == workflow.DegradedSnapshotStale && len(w.Agents) == 3
	}, 5*time.Second, "the lost socket never left the workflow snapshot_stale with its rows")
	w := boardEntry(sess, probeTask)
	if w.Name != "probe" || w.StartedAt != probeStartedAt || w.RunID != wfRun || w.Status != workflow.StatusRunning {
		t.Fatalf("after the restart: %q started %d run %q %s; want the Ref's name, start and run id, still running", w.Name, w.StartedAt, w.RunID, w.Status)
	}
	if !b.Running() {
		t.Error("a stale running workflow must still read as running")
	}

	r.ReconnectShimsCtx(context.Background())
	testhelper.Eventually(t, func() bool {
		w := boardEntry(sess, probeTask)
		return w != nil && w.Status == workflow.StatusCompleted
	}, 5*time.Second, "the reattached CLI's terminal frames never reached the board")
	w = boardEntry(sess, probeTask)
	if w.Degraded != "" || w.Name != "probe" || sess.WorkflowBoard() != b || b.Published().Epoch != epoch {
		t.Errorf("after reattaching: degraded %q name %q, same board %v; want live, the Ref's name, one board and epoch", w.Degraded, w.Name, sess.WorkflowBoard() == b)
	}
}

// readLoops counts the cli read loops running in the test binary.
func readLoops() int {
	buf := make([]byte, 8<<20)
	buf = buf[:runtime.Stack(buf, true)]
	return strings.Count(string(buf), "cli.(*Process).readLoop(")
}

// waitReadLoops waits, at most 5s, until at most n read loops run.
func waitReadLoops(n int) {
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	deadline := time.After(5 * time.Second)
	for readLoops() > n {
		select {
		case <-tick.C:
		case <-deadline:
			return
		}
	}
}

// TestReconnectShims_WorkflowEndsWithItsCLI: a shim killed under a running
// naozhi (the socket breaks and the shim is gone) and a CLI that exits both
// end the run as interrupted at once (§5.6(6a)), also when the read loop
// exits before the reattach commits: the end is then delivered inside
// bookProcessEnd, which must come after the bind.
func TestReconnectShims_WorkflowEndsWithItsCLI(t *testing.T) {
	lines := probeLines(t)
	for _, tc := range []struct {
		name     string
		sc       shimScript
		endFirst bool
	}{
		{"shim killed", shimScript{firstSeq: 1, replay: lines[3:11], hangUp: true, shimPID: FakeShimPID}, false},
		{"cli exited", shimScript{firstSeq: 1, replay: lines[3:11], exit: true, shimPID: os.Getpid()}, false},
		{"cli exited before the commit", shimScript{firstSeq: 1, replay: lines[3:11], exit: true, shimPID: os.Getpid()}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, sess := workflowShim(t, &storeEntry{Key: "feishu:direct:alice:general", SessionID: resolveStoredSID}, tc.sc)
			if tc.endFirst {
				// The history read sits between the spawn and the commit.
				base := readLoops()
				r.hist.loader.(*racingHistoryLoader).duringRead = func() { waitReadLoops(base) }
			}
			r.ReconnectShimsCtx(context.Background())
			if sess.loadProcess() == nil {
				t.Fatal("premise: no reattach")
			}
			testhelper.Eventually(t, func() bool {
				w := boardEntry(sess, probeTask)
				return w != nil && w.Status == workflow.StatusInterrupted
			}, 5*time.Second, "the run did not end with its CLI")
			if w := boardEntry(sess, probeTask); w.Name != "probe" || w.Agents[2].State != workflow.AgentStopped {
				t.Errorf("interrupted %q with agent C %s, want the probe's name and its live agent stopped", w.Name, w.Agents[2].State)
			}
			if boundProc(sess.WorkflowBoard()) != nil {
				t.Error("the board stays bound to an ended process")
			}
		})
	}
}

// TestReconnectShims_AbandonedReattachDoesNotPublish: a reattach given up
// because a Send still holds the session closes its process; the board was
// never bound to it, so nothing that process replayed is published.
func TestReconnectShims_AbandonedReattachDoesNotPublish(t *testing.T) {
	lines := probeLines(t)
	r, sess := workflowShim(t, &storeEntry{Key: "feishu:direct:alice:general", SessionID: resolveStoredSID},
		shimScript{firstSeq: 1, replay: lines[3:11], shimPID: os.Getpid()})
	sess.sendMu.Lock()
	r.ReconnectShimsCtx(context.Background())
	sess.sendMu.Unlock()
	if sess.loadProcess() != nil {
		t.Fatal("premise: the reattach went through")
	}
	if got := sess.WorkflowBoard().Published().Workflows; len(got) != 0 {
		t.Errorf("an abandoned reattach published %v", taskIDs(got))
	}
}

// TestReconnectShims_KnownTasksSeedTheReplay: a ring holding only the run's
// terminal frames builds its entry because the board hands the Tracker its
// task ids before the seed (§5.9 R1); the Ref fills in the rest. The bind
// wires the router's sessions_update closures.
func TestReconnectShims_KnownTasksSeedTheReplay(t *testing.T) {
	lines := probeLines(t)
	r, sess := workflowShim(t, &storeEntry{Key: "feishu:direct:alice:general", SessionID: resolveStoredSID, Workflows: probeRef()},
		shimScript{firstSeq: 9001, replay: lines[14:16], shimPID: os.Getpid()})
	sess.WorkflowBoard().setNotify(nil, nil) // the reattach wires its own
	r.ReconnectShimsCtx(context.Background())
	if sess.loadProcess() == nil {
		t.Fatal("premise: no reattach")
	}
	notifyWired(t, r, sess)
	w := boardEntry(sess, probeTask)
	if w == nil || w.Status != workflow.StatusCompleted || w.Name != "probe" || w.Source != workflow.SourceReplay {
		t.Fatalf("entry %+v, want the replay's completed status under the Ref's name", w)
	}
}
