//go:build unix

package session

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// realRunLayout writes the probe's run under a real projects root for a
// workspace reached through a symlink, as CC lays it out (slugged from the
// realpath), and returns the root, the symlinked workspace and the result
// file's path.
func realRunLayout(t *testing.T) (root, workspace, resultPath string) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(tmp, "claude", "projects")
	realWS := filepath.Join(tmp, "private", "ws")
	if err := os.MkdirAll(realWS, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tmp, "private"), filepath.Join(tmp, "tmp")); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	projectDir := filepath.Join(root, claudefs.ProjectSlug(realWS))
	if err := os.MkdirAll(claudefs.WorkflowRunDir(claudefs.SubagentsDir(projectDir, wfSID), wfRun), 0o755); err != nil {
		t.Fatal(err)
	}
	resultPath = claudefs.WorkflowResultFile(projectDir, wfSID, wfRun)
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "cli", "workflow", "testdata", "run", wfRun+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(tmp, "tmp", "ws"), resultPath
}

// TestWorkflowBoard_RealDiskSymlinkWorkspace is the restart acceptance on a
// real filesystem: with no shim left, a restored terminal workflow of a
// session whose workspace is spelled through a symlink resolves its run dir
// from the session id and run id alone and shows all its rows.
func TestWorkflowBoard_RealDiskSymlinkWorkspace(t *testing.T) {
	root, ws, _ := realRunLayout(t)
	r := newWFRig(t)
	r.b.projectsRoot, r.b.disk = root, workflowDiskFS
	r.b.restore("k", []workflow.Ref{{TaskID: probeTask, RunID: wfRun, Name: "probe", SessionID: wfSID, Status: workflow.StatusCompleted, EndedAt: wfT0}}, ws, r.now())
	testhelper.Eventually(t, func() bool { return len(r.entry(t, probeTask).Agents) == 3 }, 5*time.Second, "the restored workflow never got its rows")
	w := r.entry(t, probeTask)
	wantDir := claudefs.WorkflowRunDir(claudefs.SubagentsDir(filepath.Join(root, claudefs.ProjectSlug(filepath.Join(filepath.Dir(filepath.Dir(ws)), "private", "ws"))), wfSID), wfRun)
	if w.RunDir != wantDir || w.Source != workflow.SourceResultFile || r.b.cachedResult(probeTask) == nil {
		t.Errorf("RunDir %q (want %q) source %s cached %v", w.RunDir, wantDir, w.Source, r.b.cachedResult(probeTask) != nil)
	}
}

// TestWorkflowBoard_RealDiskResultFIFO: a FIFO planted as the result file
// fails the read at once; the board's I/O drains and the sweeper keeps
// going instead of a slot parking on the FIFO forever.
func TestWorkflowBoard_RealDiskResultFIFO(t *testing.T) {
	root, ws, resultPath := realRunLayout(t)
	if err := os.Remove(resultPath); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(resultPath, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	t.Cleanup(func() {
		// Unpark a read that did block, so the test binary can exit.
		if w, err := os.OpenFile(resultPath, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
	})
	r := newWFRig(t)
	r.b.projectsRoot, r.b.disk = root, workflowDiskFS
	r.b.restore("k", []workflow.Ref{{TaskID: probeTask, RunID: wfRun, SessionID: wfSID, Status: workflow.StatusRunning, LastObservedAt: wfT0}}, ws, r.now())
	testhelper.Eventually(t, func() bool { return r.entry(t, probeTask).RunDir != "" }, 5*time.Second, "run dir never resolved")
	for range 4 {
		r.sweepAfter(t, 30*time.Second)
	}
	if w := r.entry(t, probeTask); w.Status != workflow.StatusInterrupted || len(w.Agents) != 0 {
		t.Errorf("status %s, %d rows; want the orphan interrupted past the FIFO", w.Status, len(w.Agents))
	}
}

// TestWorkflowBoard_RealDiskRunDirAfterReceipt: the launch receipt can be
// read before CC has created the run dir. The first resolution fails, a
// later sweep finds the dir, and the run whose terminal frame never came
// settles from its result file.
func TestWorkflowBoard_RealDiskRunDirAfterReceipt(t *testing.T) {
	root, _, resultPath := realRunLayout(t)
	projectDir := filepath.Dir(filepath.Dir(filepath.Dir(resultPath)))
	runDir := claudefs.WorkflowRunDir(claudefs.SubagentsDir(projectDir, wfSID), wfRun)
	hidden := runDir + ".hidden"
	if err := os.Rename(runDir, hidden); err != nil {
		t.Fatal(err)
	}
	r := newWFRig(t)
	r.b.projectsRoot, r.b.disk = root, workflowDiskFS
	p := &setProc{}
	w := runningWithRun(probeTask, wfRun)
	w.LaunchTranscriptDir = runDir
	p.publish(w)
	r.b.bind(p, "/elsewhere")
	r.settleIO(t)
	if got := r.entry(t, probeTask).RunDir; got != "" {
		t.Fatalf("run dir %q resolved before it existed", got)
	}
	if err := os.Rename(hidden, runDir); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		r.sweepAfter(t, 30*time.Second)
	}
	testhelper.Eventually(t, func() bool { return r.entry(t, probeTask).Status == workflow.StatusCompleted }, 5*time.Second, "the run never settled once its dir appeared")
	if got := r.entry(t, probeTask).RunDir; got != runDir {
		t.Errorf("run dir %q, want %q", got, runDir)
	}
}
