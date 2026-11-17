package shim

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// A line the CLI writes while a reattaching client's replay is still going
// out lands in the ring after the replay snapshot and before admission; it
// must still reach that client, or a lost turn result leaves the session
// running until the watchdog.
func TestHandleClient_LineDuringReplayIsDelivered(t *testing.T) {
	dir := shortSocketDir(t)
	tokenRaw, _, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	cli, err := startCLI("sh", []string{"-c", "cat"}, dir)
	if err != nil {
		t.Fatalf("startCLI: %v", err)
	}
	defer cli.kill()

	// A replay far past any unix socket buffer, so the shim stays blocked
	// mid-replay (after its snapshot, before admission) while the client is
	// not reading.
	const prefill = 200
	buf := NewRingBuffer(1000, 64<<20)
	pad := bytes.Repeat([]byte("x"), 64<<10)
	for range prefill {
		buf.Push(pad)
	}
	stateFile := filepath.Join(dir, "state.json")
	s := &shimServer{
		cli:       cli,
		buffer:    buf,
		tokenRaw:  tokenRaw,
		stateFile: stateFile,
		state:     State{Key: "tail:key", CLIAlive: true},
		done:      make(chan struct{}),
	}
	s.watchdog = NewWatchdog(30*time.Second, nil)
	WriteStateFile(stateFile, s.state) //nolint:errcheck
	go s.readStdout()

	sock := filepath.Join(dir, "tail.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	clientConn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	serverConn := <-accepted
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		s.handleClient(serverConn, 5*time.Second)
	}()
	defer func() { clientConn.Close(); <-handlerDone }()

	clientConn.SetDeadline(time.Now().Add(8 * time.Second)) //nolint:errcheck
	attach, _ := json.Marshal(ClientMsg{Type: "attach", Token: base64.StdEncoding.EncodeToString(tokenRaw)})
	if _, err := clientConn.Write(append(attach, '\n')); err != nil {
		t.Fatalf("write attach: %v", err)
	}
	reader := bufio.NewReader(clientConn)
	read := func() ServerMsg {
		t.Helper()
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var m ServerMsg
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("parse: %v", err)
		}
		return m
	}
	if m := read(); m.Type != "hello" {
		t.Fatalf("first frame %q, want hello", m.Type)
	}
	if m := read(); m.Type != "replay" || m.Seq != 1 {
		t.Fatalf("frame %s seq %d, want replay seq 1", m.Type, m.Seq)
	}

	// The shim is blocked further on in the replay; the CLI writes a line.
	if _, err := cli.stdin.Write([]byte("live\n")); err != nil {
		t.Fatalf("write CLI stdin: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, end := buf.SeqRange(); end >= prefill+1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CLI line never reached the ring")
		}
		time.Sleep(5 * time.Millisecond)
	}

	for {
		m := read()
		if m.Type == "replay" && m.Seq <= prefill || m.Type == "replay_done" {
			continue
		}
		if m.Seq != prefill+1 || m.Line != "live" {
			t.Fatalf("frame %s seq %d, want the CLI's line at seq %d", m.Type, m.Seq, prefill+1)
		}
		return
	}
}
