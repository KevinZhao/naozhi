package shim

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func TestStderrTail_KeepsLastLines(t *testing.T) {
	t.Parallel()
	var tail StderrTail
	if got := tail.Lines(); got != nil {
		t.Fatalf("empty Lines() = %q, want nil", got)
	}
	for i := 1; i <= StderrTailLines+3; i++ {
		tail.Push(fmt.Sprintf("line %d", i))
		tail.Push("  ") // blank lines must not push error text out
	}
	got := tail.Lines()
	want := make([]string, 0, StderrTailLines)
	for i := 4; i <= StderrTailLines+3; i++ {
		want = append(want, fmt.Sprintf("line %d", i))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Lines() = %q, want %q", got, want)
	}
	got[0] = "mutated"
	if tail.Lines()[0] == "mutated" {
		t.Fatal("Lines() aliases the ring")
	}

	tail.Replace([]string{"a", "", "b"})
	if got := tail.Lines(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("after Replace Lines() = %q, want [a b]", got)
	}
}

func TestStderrTail_CapsLineOnRuneBoundary(t *testing.T) {
	t.Parallel()
	var tail StderrTail
	// "é" is 2 bytes; an odd prefix puts the byte cap mid-rune.
	tail.Push("x" + strings.Repeat("é", StderrTailLineBytes))
	got := tail.Lines()[0]
	if len(got) > StderrTailLineBytes || len(got) < StderrTailLineBytes-1 {
		t.Fatalf("kept line is %d bytes, want ~%d", len(got), StderrTailLineBytes)
	}
	if !strings.HasSuffix(got, "é") {
		t.Fatalf("line cut mid-rune: tail %q", got[len(got)-4:])
	}
}

func TestStderrTail_CappedLineDoesNotPinInput(t *testing.T) {
	t.Parallel()
	var tail StderrTail
	long := strings.Repeat("x", 1<<20)
	tail.Push(long)
	got := tail.Lines()[0]
	if len(got) != StderrTailLineBytes {
		t.Fatalf("kept line is %d bytes, want %d", len(got), StderrTailLineBytes)
	}
	if unsafe.StringData(got) == unsafe.StringData(long) {
		t.Fatal("capped line shares the input's backing array")
	}
}

// nodeCrashStderr is an uncaught node exception: the cause line is followed by
// more than StderrTailLines lines of stack and trailer.
var nodeCrashStderr = []string{
	"node:internal/modules/cjs/loader:1228",
	"  throw err;",
	"  ^",
	"",
	"Error: Cannot find module '/opt/claude/cli.js'",
	"    at Module._resolveFilename (node:internal/modules/cjs/loader:1225:15)",
	"    at Module._load (node:internal/modules/cjs/loader:1051:27)",
	"    at Function.executeUserEntryPoint [as runMain] (node:internal/modules/run_main:142:12)",
	"    at node:internal/main/run_main_module:28:49 {",
	"  code: 'MODULE_NOT_FOUND',",
	"  requireStack: []",
	"}",
	"",
	"Node.js v20.10.0",
}

func TestStderrTail_KeepsFirstErrorLine(t *testing.T) {
	t.Parallel()
	var tail StderrTail
	for _, l := range nodeCrashStderr {
		tail.Push(l)
	}
	// The cause takes the oldest slot, so the tail stays StderrTailLines long.
	want := []string{
		"Error: Cannot find module '/opt/claude/cli.js'",
		"    at Module._load (node:internal/modules/cjs/loader:1051:27)",
		"    at Function.executeUserEntryPoint [as runMain] (node:internal/modules/run_main:142:12)",
		"    at node:internal/main/run_main_module:28:49 {",
		"  code: 'MODULE_NOT_FOUND',",
		"  requireStack: []",
		"}",
		"Node.js v20.10.0",
	}
	if got := tail.Lines(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Lines() = %q,\nwant %q", got, want)
	}

	// While the cause is still in the window nothing is moved, and Replace
	// forgets the old cause.
	tail.Replace([]string{"TypeError: x is not a function", "    at f (a.js:1:1)"})
	if got := tail.Lines(); !reflect.DeepEqual(got, []string{"TypeError: x is not a function", "    at f (a.js:1:1)"}) {
		t.Fatalf("after Replace Lines() = %q", got)
	}
	var frames []string
	for i := 1; i <= StderrTailLines+4; i++ {
		frames = append(frames, fmt.Sprintf("    at f%d (a.js:1:1)", i))
	}
	tail.Replace(frames)
	if got := tail.Lines(); got[0] != frames[4] {
		t.Fatalf("Replace kept a stale cause: Lines()[0] = %q, want %q", got[0], frames[4])
	}

	// The first error line is the cause, not a later one.
	tail.Replace(append(append([]string{"Error: first"}, frames...), "Error: second"))
	if got := tail.Lines(); got[0] != "Error: first" {
		t.Fatalf("Lines()[0] = %q, want the first error line", got[0])
	}
}

func TestIsStderrErrorLine(t *testing.T) {
	t.Parallel()
	for line, want := range map[string]bool{
		"Error: No conversation found with session ID: abc":   true,
		"TypeError: Cannot read properties of undefined":      true,
		"  Error [ERR_MODULE_NOT_FOUND]: Cannot find package": true,
		"error: Cannot find module 'x' from '/b'":             true,
		"node:internal/modules/cjs/loader:1228":               false,
		"    at Module._load (node:internal/loader:1051:27)":  false,
		"Some Error: spaced label":                            false,
		"Errors: 3":                                           false,
		"no colon Error":                                      false,
		": Error":                                             false,
	} {
		if got := IsStderrErrorLine(line); got != want {
			t.Errorf("IsStderrErrorLine(%q) = %v, want %v", line, got, want)
		}
	}
}

// stderrTestShim starts script as the CLI with readStdout / readStderr
// running, as Run does, and a listener for attachForExit.
func stderrTestShim(t *testing.T, script string) (*shimServer, net.Listener, []byte) {
	t.Helper()
	dir := shortSocketDir(t)
	socketPath := filepath.Join(dir, "tail.sock")
	stateFile := filepath.Join(dir, "tail_state.json")
	tokenRaw, _, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	cli, err := startCLI("sh", []string{"-c", script}, dir)
	if err != nil {
		t.Fatalf("startCLI: %v", err)
	}
	t.Cleanup(cli.kill)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &shimServer{
		cli:       cli,
		listener:  ln,
		buffer:    NewRingBuffer(100, 1024*1024),
		tokenRaw:  tokenRaw,
		stateFile: stateFile,
		state:     State{Key: "tail:key", Socket: socketPath, CLIAlive: true},
		done:      make(chan struct{}),
	}
	s.watchdog = NewWatchdog(30*time.Second, nil)
	WriteStateFile(stateFile, s.state) //nolint:errcheck
	go s.readStdout()
	go s.readStderr()
	return s, ln, tokenRaw
}

// attachForExit attaches a client, optionally sends one write, and returns
// the cli_exited frame. It waits for handleClient so its deferred saveState
// is done before the temp dir is removed.
func attachForExit(t *testing.T, s *shimServer, ln net.Listener, tokenRaw []byte, write string) ServerMsg {
	t.Helper()
	connCh := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			connCh <- c
		}
	}()
	conn, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	var serverConn net.Conn
	select {
	case serverConn = <-connCh:
	case <-time.After(2 * time.Second):
		t.Fatal("accept timeout")
	}
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		s.handleClient(serverConn, 5*time.Second)
	}()
	defer func() {
		s.initiateShutdown()
		select {
		case <-handlerDone:
		case <-time.After(5 * time.Second):
			t.Error("handleClient did not return")
		}
	}()

	c := connectedClient{conn: conn, reader: bufio.NewReader(conn), writer: bufio.NewWriter(conn)}
	sendClientCmd(t, c, ClientMsg{Type: "attach", Token: base64.StdEncoding.EncodeToString(tokenRaw)})
	conn.SetReadDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	for {
		line, err := c.reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("no cli_exited frame: %v", err)
		}
		var msg ServerMsg
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "replay_done":
			// The handshake reader may buffer past the attach line, so a
			// command is only sent once the command loop is reading.
			if write != "" {
				sendClientCmd(t, c, ClientMsg{Type: "write", Line: write})
			}
		case "cli_exited":
			return msg
		}
	}
}

func waitCLIExited(t *testing.T, s *shimServer, within time.Duration) {
	t.Helper()
	select {
	case <-s.cli.exited:
	case <-time.After(within):
		t.Fatalf("CLI not reaped within %v", within)
	}
}

// A CLI that fails before naozhi attaches: its stderr frames were dropped
// (no client), so cli_exited is the only place the cause can travel.
func TestCLIExited_CarriesStderrWrittenBeforeAttach(t *testing.T) {
	t.Parallel()
	s, ln, tok := stderrTestShim(t,
		`printf 'starting\nError: No conversation found with session ID: abc\nhint: run /new\n' >&2; exit 1`)
	waitCLIExited(t, s, 5*time.Second)

	msg := attachForExit(t, s, ln, tok, "")
	if msg.Code == nil || *msg.Code != 1 {
		t.Errorf("cli_exited code = %v, want 1", msg.Code)
	}
	want := []string{"starting", "Error: No conversation found with session ID: abc", "hint: run /new"}
	if !reflect.DeepEqual(msg.StderrTail, want) {
		t.Errorf("stderr_tail = %q, want %q", msg.StderrTail, want)
	}
}

// The live-exit writer (CLI dies while a client is attached) carries the tail too.
func TestCLIExited_LiveExitCarriesStderrTail(t *testing.T) {
	t.Parallel()
	s, ln, tok := stderrTestShim(t, `read l; echo "Error: Invalid API key" >&2; exit 2`)
	msg := attachForExit(t, s, ln, tok, `{"type":"user"}`)
	if msg.Code == nil || *msg.Code != 2 {
		t.Errorf("cli_exited code = %v, want 2", msg.Code)
	}
	if !reflect.DeepEqual(msg.StderrTail, []string{"Error: Invalid API key"}) {
		t.Errorf("stderr_tail = %q, want [Error: Invalid API key]", msg.StderrTail)
	}
}

// Reaping closes the stderr pipe, so the CLI's last lines (its error message)
// must be read first. Here stdout is already at EOF and the process has
// exited, but stderr still has a line on the way.
func TestReadStdout_ReapsAfterStderrDrained(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	stderrR, stderrW := io.Pipe()
	s := &shimServer{
		cli: &cliProc{
			cmd:        cmd,
			stdout:     bufio.NewScanner(strings.NewReader("")),
			stderrR:    stderrR,
			stderrDone: make(chan struct{}),
			exited:     make(chan struct{}),
		},
		buffer: NewRingBuffer(10, 1024),
		done:   make(chan struct{}),
	}
	go s.readStderr()
	go s.readStdout()
	time.AfterFunc(100*time.Millisecond, func() {
		stderrW.Write([]byte("Error: written last\n")) //nolint:errcheck
		stderrW.Close()
	})
	waitCLIExited(t, s, 3*time.Second)
	if got := s.stderrTail.Lines(); !reflect.DeepEqual(got, []string{"Error: written last"}) {
		t.Fatalf("tail when the CLI was reaped = %q, want [Error: written last]", got)
	}
}

// A grandchild that inherited stderr keeps the pipe open after the CLI exits;
// the stderr wait is bounded so reaping (and cli_exited) is not held up.
func TestReadStdout_GrandchildHoldingStderrDoesNotStallReap(t *testing.T) {
	t.Parallel()
	s, _, _ := stderrTestShim(t, `sleep 10 >/dev/null & echo "Error: boom" >&2; exit 1`)
	waitCLIExited(t, s, 3*time.Second)
	if got := s.stderrTail.Lines(); !reflect.DeepEqual(got, []string{"Error: boom"}) {
		t.Errorf("tail = %q, want [Error: boom]", got)
	}
}
