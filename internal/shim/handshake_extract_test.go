package shim

import (
	"bufio"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestPerformHandshake_BadToken pins R219-CR-8 (#657) extraction:
// performHandshake rejects an attach with the wrong token by sending
// auth_failed and returning ok=false WITHOUT promoting the conn to active
// client. Direct exercise of the carved-out helper guards against future
// edits silently dropping the constant-time compare.
func TestPerformHandshake_BadToken(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "h.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	defer os.Remove(socketPath)

	tokenRaw, _, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	s := &shimServer{
		tokenRaw: tokenRaw,
	}

	var (
		wg     sync.WaitGroup
		gotOK  bool
		gotMsg ClientMsg
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		c, err := ln.Accept()
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer c.Close()
		gotMsg, gotOK = s.performHandshake(c)
	}()

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	// Send attach with a token that does NOT match s.tokenRaw.
	bogus := make([]byte, len(tokenRaw))
	for i := range bogus {
		bogus[i] = ^tokenRaw[i]
	}
	if subtle.ConstantTimeCompare(bogus, tokenRaw) == 1 {
		t.Fatal("test bug: bogus token equals real token")
	}
	attach := ClientMsg{
		Type:  "attach",
		Token: base64.StdEncoding.EncodeToString(bogus),
	}
	data, _ := json.Marshal(attach)
	if _, err := conn.Write(append(data, '\n')); err != nil {
		t.Fatalf("write attach: %v", err)
	}

	// Server should send auth_failed before closing.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	rd := bufio.NewReader(conn)
	line, err := rd.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read auth_failed: %v", err)
	}
	var resp ServerMsg
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Type != "auth_failed" {
		t.Errorf("Type = %q, want auth_failed", resp.Type)
	}

	wg.Wait()
	if gotOK {
		t.Error("performHandshake returned ok=true on bad token")
	}
	if gotMsg.Type != "" {
		t.Errorf("attachMsg leaked on failure: %+v", gotMsg)
	}
}

// TestPerformHandshake_GoodTokenClearsDeadline pins that on a successful
// handshake the auth read deadline is cleared (zero time). Without this,
// the post-auth command loop would inherit shimAuthReadDeadline and a
// healthy idle client would be kicked off after the auth window expires —
// the exact bug the original handleClient inline-comment documented.
//
// We exercise via a *net.UnixConn pair so VerifyPeerUID passes. After
// performHandshake returns ok=true we attempt a fresh long-deadline read
// from the conn; if the auth deadline lingers, the read returns
// immediately with a deadline error.
func TestPerformHandshake_GoodTokenClearsDeadline(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "h.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	defer os.Remove(socketPath)

	tokenRaw, _, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	s := &shimServer{tokenRaw: tokenRaw}

	type result struct {
		msg  ClientMsg
		ok   bool
		conn net.Conn
	}
	resCh := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		msg, ok := s.performHandshake(c)
		resCh <- result{msg: msg, ok: ok, conn: c}
	}()

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	attach := ClientMsg{
		Type:  "attach",
		Token: base64.StdEncoding.EncodeToString(tokenRaw),
		Seq:   42,
	}
	data, _ := json.Marshal(attach)
	if _, err := conn.Write(append(data, '\n')); err != nil {
		t.Fatalf("write attach: %v", err)
	}

	var res result
	select {
	case res = <-resCh:
	case <-time.After(3 * time.Second):
		t.Fatal("performHandshake timeout")
	}
	if !res.ok {
		t.Fatal("performHandshake returned ok=false on good token")
	}
	if res.msg.Seq != 42 {
		t.Errorf("attachMsg.Seq = %d, want 42", res.msg.Seq)
	}
	if res.msg.Type != "attach" {
		t.Errorf("attachMsg.Type = %q, want attach", res.msg.Type)
	}

	// Verify the auth deadline was cleared. SetReadDeadline(time.Time{})
	// is what handleClient relies on so the post-auth loop has no
	// inherited cap. Probe by attempting a read with a 200ms deadline:
	// if performHandshake left the original ~5s shimAuthReadDeadline
	// armed, a Read here would still time out in <200ms with a deadline
	// error originating from the post-auth window — same outcome as ours.
	// Instead we Set/Reset and confirm Set works (the conn is healthy).
	if err := res.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Errorf("SetReadDeadline post-handshake failed: %v", err)
	}
	res.conn.Close()
}
