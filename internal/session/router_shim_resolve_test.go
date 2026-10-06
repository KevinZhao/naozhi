package session

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/shim"
)

const (
	resolveStoredSID = "11111111-1111-4111-8111-111111111111"
	resolveHelloSID  = "22222222-2222-4222-8222-222222222222"

	resolveToolUse = `{"parentUuid":"u1","isSidechain":false,"message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}],"stop_reason":"tool_use"},"type":"assistant","uuid":"a1"}`
	resolveEndTurn = `{"parentUuid":"u2","isSidechain":false,"message":{"role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"},"type":"assistant","uuid":"a2"}`
	resolveMeta    = `{"type":"last-prompt","lastPrompt":"go","leafUuid":"a2"}`
)

// writeMainTranscript puts body at sid's main transcript for ws; large pads
// it to a realistic multi-MiB file first.
func writeMainTranscript(t *testing.T, claudeDir, ws, sid, body string, large bool) {
	t.Helper()
	path := claudefs.SessionJSONL(claudeDir, ws, sid)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if large {
		body = strings.Repeat(resolveToolUse+"\n", (5<<20)/len(resolveToolUse)) + body
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReconnectHooks_ResolveUnknownReadsTheMainTranscript: the resolver
// SpawnReconnect consults for an unknown verdict reads the session's own
// transcript — the shim's session id first, the stored one otherwise — and
// treats anything it cannot read or decide on as a running turn.
func TestReconnectHooks_ResolveUnknownReadsTheMainTranscript(t *testing.T) {
	claudeDir := t.TempDir()
	ws := filepath.Join(t.TempDir(), "ws")
	ended := resolveToolUse + "\n" + resolveEndTurn + "\n" + resolveMeta + "\n"
	running := resolveEndTurn + "\n" + resolveToolUse + "\n"
	writeMainTranscript(t, claudeDir, ws, resolveHelloSID, ended, true)
	writeMainTranscript(t, claudeDir, ws, resolveStoredSID, running, false)
	// An ended transcript where a traversing id would land, so only the id
	// check keeps the resolver from reading it.
	escaped := filepath.Join(filepath.Dir(claudefs.ProjectDir(claudeDir, ws)), resolveHelloSID+".jsonl")
	if err := os.WriteFile(escaped, []byte(ended), 0o600); err != nil {
		t.Fatal(err)
	}

	resolve := func(backendID, workspace, stored string) func(string) bool {
		return reconnectHooks(claudeDir, nil, backendID, workspace, stored, nil).ResolveUnknown
	}
	cases := []struct {
		name    string
		resolve func(string) bool
		hello   string
		want    bool
	}{
		{"hello sid wins (5MiB transcript ending in end_turn)", resolve("claude", ws, resolveStoredSID), resolveHelloSID, true},
		{"hello sid over a stored one that is running", resolve("claude", ws, resolveHelloSID), resolveStoredSID, false},
		{"stored sid when the shim names none", resolve("claude", ws, resolveHelloSID), "", true},
		{"default backend is claude", resolve("", ws, resolveStoredSID), resolveHelloSID, true},
		{"no transcript", resolve("claude", ws, "33333333-3333-4333-8333-333333333333"), "", false},
		{"invalid session id is never a path", resolve("claude", ws, "../"+resolveHelloSID), "", false},
		{"no workspace", resolve("claude", "", resolveStoredSID), resolveHelloSID, false},
		{"backend whose transcripts naozhi does not read", resolve("kiro", ws, resolveStoredSID), resolveHelloSID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.resolve(tc.hello); got != tc.want {
				t.Errorf("ResolveUnknown(%q) = %v, want %v", tc.hello, got, tc.want)
			}
		})
	}
}

const (
	resolveProgress = `{"type":"system","subtype":"task_progress","task_id":"w113pvmto","tool_use_id":"toolu_1","description":"Ask: A","usage":{"total_tokens":10},"uuid":"u1","session_id":"s1"}`
	resolveResult   = `{"duration_api_ms":1,"stop_reason":"end_turn","session_id":"s1","subtype":"success","result":"ok","type":"result"}`
)

// reconnectReplaying reattaches a session in workspace ws, stored under
// resolveStoredSID, to a fake shim replaying backlog from seq firstSeq, and
// returns the reattached process. transcript, when set, is the session's
// main transcript.
func reconnectReplaying(t *testing.T, transcript string, firstSeq int64, backlog []string) *cli.Process {
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
	claudeDir, ws := filepath.Join(dir, "claude"), filepath.Join(dir, "ws")
	r := NewRouter(RouterConfig{Wrapper: w, ClaudeDir: claudeDir, HistoryLoader: &racingHistoryLoader{}})
	t.Cleanup(r.Shutdown)
	sess := injectSession(r, "feishu:direct:alice:general", nil)
	sess.setWorkspace(ws)
	if transcript != "" {
		writeMainTranscript(t, claudeDir, ws, resolveStoredSID, transcript, true)
	}

	t.Setenv("XDG_RUNTIME_DIR", dir)
	socket := shim.SocketPath(shim.KeyHash(sess.key))
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		t.Cleanup(func() { conn.Close() })
		rd := bufio.NewReader(conn)
		if _, err := rd.ReadBytes('\n'); err != nil { // attach
			return
		}
		frames := []shim.ServerMsg{{Type: "hello", ProtocolVersion: shim.ProtocolVersion, ShimPID: FakeShimPID}}
		for i, line := range backlog {
			frames = append(frames, shim.ServerMsg{Type: "replay", Seq: firstSeq + int64(i), Line: line})
		}
		frames = append(frames, shim.ServerMsg{Type: "replay_done", Count: len(backlog)})
		for i := range frames {
			data, _ := frames[i].MarshalLine()
			if _, err := conn.Write(data); err != nil {
				return
			}
		}
		io.Copy(io.Discard, rd) //nolint:errcheck // nothing needs an answer
	}()
	writeShimStateFor(t, dir, shim.State{
		ShimPID: os.Getpid(), Socket: socket, Key: sess.key, Backend: "claude", SessionID: resolveStoredSID,
		CLIArgs:      driftArgsFor(r).driftCompareArgs(w, "claude", sess.key, sess, &shim.SpawnOverlay{}),
		SpawnOverlay: &shim.SpawnOverlay{},
	})

	r.ReconnectShimsCtx(context.Background())
	proc, ok := sess.loadProcess().(*cli.Process)
	if !ok || proc == nil {
		t.Fatalf("session process after reconnect = %T, want the reattached *cli.Process", sess.loadProcess())
	}
	return proc
}

// TestReconnectShims_BackgroundWorkflowDoesNotPinRunning is §14 PR-3's
// acceptance: an idle session whose background workflow kept writing comes
// back Ready, whether or not the shim ring wrapped, while a foreground tool
// buried under the same flood still comes back Running.
func TestReconnectShims_BackgroundWorkflowDoesNotPinRunning(t *testing.T) {
	flood := make([]string, 200)
	for i := range flood {
		flood[i] = resolveProgress
	}
	cases := []struct {
		name       string
		transcript string
		firstSeq   int64
		backlog    []string
		want       cli.ProcessState
	}{
		{"idle, ring intact", "", 1, append([]string{resolveResult}, flood...), cli.StateReady},
		{"idle, ring wrapped: the transcript decides", resolveEndTurn + "\n" + resolveMeta + "\n", 9001, flood, cli.StateReady},
		{"foreground Bash, ring wrapped", resolveEndTurn + "\n" + resolveToolUse + "\n", 9001, flood, cli.StateRunning},
		{"ring wrapped, no transcript", "", 9001, flood, cli.StateRunning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reconnectReplaying(t, tc.transcript, tc.firstSeq, tc.backlog).State(); got != tc.want {
				t.Errorf("State after reconnect = %v, want %v", got, tc.want)
			}
		})
	}
}
