package session

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/shim"
	"github.com/naozhi/naozhi/internal/subagent"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// The shim replay walk hands the SubagentLinker only tasks with an agent
// transcript: a Workflow run (CC 2.1.288 probe frame) and local_bash are
// skipped, agents resolve once per task_id.
func TestReplayLinkerTasks_SkipsWorkflowAndBash(t *testing.T) {
	t.Parallel()
	const (
		workflowStarted = `{"type":"system","subtype":"task_started","task_id":"w113pvmto","tool_use_id":"toolu_bdrk_011p2dbLZ9gzoWeQA1qtTGau","description":"tiny probe","task_type":"local_workflow","workflow_name":"probe","session_id":"04a8fc10-6fa5-4b8e-82ba-621974425917"}`
		agentStarted    = `{"type":"system","subtype":"task_started","task_id":"a7k2m9p4q","tool_use_id":"toolu_agent_1","description":"Explore: map the repo","task_type":"local_agent","session_id":"04a8fc10-6fa5-4b8e-82ba-621974425917"}`
		teammateStarted = `{"type":"system","subtype":"task_started","task_id":"t1","tool_use_id":"toolu_team_1","description":"reviewer: check","task_type":"in_process_teammate"}`
		bashStarted     = `{"type":"system","subtype":"task_started","task_id":"b4n8c2x7z","tool_use_id":"toolu_bash_1","description":"npm test","task_type":"local_bash"}`
		stdoutStarted   = `{"type":"system","subtype":"task_started","task_id":"t2","tool_use_id":"toolu_team_2","description":"live: not a replay","task_type":"in_process_teammate"}`
		noToolUseID     = `{"type":"system","subtype":"task_started","task_id":"a0000000z","description":"x","task_type":"local_agent"}`
	)
	replays := []shim.ServerMsg{
		{Type: "replay", Line: workflowStarted},
		{Type: "replay", Line: agentStarted},
		{Type: "replay", Line: bashStarted},
		{Type: "replay", Line: agentStarted},
		{Type: "stdout", Line: stdoutStarted},
		{Type: "replay", Line: noToolUseID},
		{Type: "replay", Line: "not json"},
		{Type: "replay", Line: teammateStarted},
	}
	var got []string
	for _, ev := range replayLinkerTasks(&cli.ClaudeProtocol{}, replays) {
		got = append(got, ev.TaskID+"/"+ev.ToolUseID)
	}
	if want := []string{"a7k2m9p4q/toolu_agent_1", "t1/toolu_team_1"}; !slices.Equal(got, want) {
		t.Errorf("replayLinkerTasks = %v, want %v", got, want)
	}
}

// TestReconnectShims_ReplayLinksOnlyAgentTasks reattaches a live shim whose
// replay holds a workflow, a local_bash and an agent task_started, each with
// an agent-<task_id>.jsonl on disk that Resolve would link. Only the agent
// task reaches the reattached process's linker.
func TestReconnectShims_ReplayLinksOnlyAgentTasks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shim discovery needs unix PID liveness and unix sockets")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := filepath.Join(home, "ws")
	subDir := claudefs.SubagentsDir(subagent.ProjectDir(ws), "sid-1")
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"w113pvmto", "b4n8c2x7z", "a7k2m9p4q"} {
		if err := os.WriteFile(claudefs.SubagentJSONL(subDir, id), []byte(`{"sessionId":"sid-1"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
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
	sess := injectSession(r, "feishu:direct:alice:general", nil)
	sess.setWorkspace(ws)
	// Agent last: once it is linked, the skipped tasks' Resolves (kicked
	// earlier, same single stat) would have landed too.
	writeLiveShimReplay(t, dir, r, w, sess, []string{
		`{"type":"system","subtype":"task_started","task_id":"w113pvmto","tool_use_id":"toolu_wf","description":"tiny probe","task_type":"local_workflow","workflow_name":"probe","session_id":"sid-1"}`,
		`{"type":"system","subtype":"task_started","task_id":"b4n8c2x7z","tool_use_id":"toolu_bash","description":"npm test","task_type":"local_bash"}`,
		`{"type":"system","subtype":"task_started","task_id":"a7k2m9p4q","tool_use_id":"toolu_agent","description":"Explore: map the repo","task_type":"local_agent","session_id":"sid-1"}`,
		// A closing result keeps the reattached turn idle, so Shutdown does
		// not wait out ShutdownTimeout on a mid-turn verdict.
		`{"type":"result","subtype":"success","result":"ok","session_id":"sid-1"}`,
	})

	r.ReconnectShimsCtx(context.Background())

	proc, ok := sess.loadProcess().(*cli.Process)
	if !ok || proc == nil {
		t.Fatalf("precondition: session process after reconnect = %T, want the reattached *cli.Process", sess.loadProcess())
	}
	linker := proc.Linker()
	testhelper.Eventually(t, func() bool {
		info, ok := linker.Query("a7k2m9p4q")
		return ok && info.InternalAgentID != ""
	}, 5*time.Second, "replayed agent task was never linked")
	for _, id := range []string{"w113pvmto", "b4n8c2x7z"} {
		if info, ok := linker.Query(id); ok {
			t.Errorf("replayed task %s reached the linker: %+v", id, info)
		}
	}
}
