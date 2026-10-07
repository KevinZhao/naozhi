//go:build unix

package session

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/testhelper"
)

const probeAgent = "a2093755b9a9ce8c0"

// TestWorkflowBoard_AgentTranscriptReady: once the run dir resolves, the
// probe's agent transcript is ready under the projects root's spelling,
// opens to the file's bytes and names the run dir's session, also for an
// entry that knows no session id, only its launch receipt's run dir; its
// opener refuses a FIFO without blocking.
func TestWorkflowBoard_AgentTranscriptReady(t *testing.T) {
	for _, launchOnly := range []bool{false, true} {
		agentTranscriptReady(t, launchOnly)
	}
}

func agentTranscriptReady(t *testing.T, launchOnly bool) {
	root, ws, resultPath := realRunLayout(t)
	runDir := claudefs.WorkflowRunDir(claudefs.SubagentsDir(filepath.Dir(filepath.Dir(filepath.Dir(resultPath))), wfSID), wfRun)
	data, err := os.ReadFile(filepath.Join("..", "subagent", "testdata", "agent-"+probeAgent+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	path := claudefs.SubagentJSONL(runDir, probeAgent)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r := newWFRig(t)
	r.b.projectsRoot, r.b.disk = root, workflowDiskFS
	p := &setProc{}
	w := runningWithRun(probeTask, wfRun)
	w.Agents[0].AgentID = probeAgent
	if launchOnly {
		w.SessionID, w.Src.SessionID, w.LaunchTranscriptDir = "", 0, runDir
	}
	p.publish(w)
	r.b.bind(p, ws)
	testhelper.Eventually(t, func() bool { return r.entry(t, probeTask).RunDir != "" }, 5*time.Second, "run dir never resolved")

	tr, st := r.b.AgentTranscript(probeAgent)
	if st != TranscriptReady || tr.Path != path || tr.RunSessionID != wfSID || !tr.Loc.Current || tr.Loc.TaskID != probeTask {
		t.Fatalf("status %d, %+v; want ready at %s naming %s", st, tr, path, wfSID)
	}
	f, err := tr.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if string(got) != string(data) {
		t.Errorf("opened %d bytes, want the transcript's %d", len(got), len(data))
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := tr.Open()
		if f != nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, osutil.ErrNotRegular) {
			t.Errorf("FIFO: %v, want ErrNotRegular", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Open blocked on a FIFO")
		if w, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
			w.Close()
		}
	}
}
