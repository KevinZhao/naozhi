package shim

// Reconnect path: re-attaching to a shim that outlived this naozhi process —
// read its state file, dial the socket, replay from lastSeq. Split out of
// manager.go (#2713 B6).

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// reconnectKey returns (lazily creating) the per-key mutex Reconnect holds
// across read-state + dial + swap. Entries are reclaimed by Remove(key) (#2251).
func (m *Manager) reconnectKey(key string) *sync.Mutex {
	m.reconnectMu.Lock()
	defer m.reconnectMu.Unlock()
	mu, ok := m.reconnectKM[key]
	if !ok {
		mu = &sync.Mutex{}
		m.reconnectKM[key] = mu
	}
	return mu
}

// Reconnect connects to an existing shim identified by its state file; lastSeq
// is the last received sequence number for replay positioning.
//
// Not gated by pendingShims/maxShims: callers reattach sequentially to shims
// already on disk, so a gate would only fail a cold start with >maxShims files.
//
// reconnectKM[key] is held across read-state + dial + swap so two callers on
// the same key never build parallel handles and close one Router already uses.
// The old handle is captured and swapped under m.mu and closed outside m.mu
// (Close does network I/O). Lock order: reconnectKM[key] -> m.mu, never reversed.
func (m *Manager) Reconnect(ctx context.Context, key string, lastSeq int64) (*ShimHandle, error) {
	// Per-key serialise across read-state + dial + swap; cross-key stays parallel.
	rmu := m.reconnectKey(key)
	rmu.Lock()
	defer rmu.Unlock()

	keyHash := KeyHash(key)
	stateFile := StateFilePath(m.stateDir, keyHash)

	state, err := ReadStateFile(stateFile)
	if err != nil {
		return nil, fmt.Errorf("read state file: %w", err)
	}

	if !pidAlive(state.ShimPID) {
		RemoveStateFile(stateFile)
		return nil, fmt.Errorf("shim PID %d not alive", state.ShimPID)
	}

	// Binary identity: Linux reads /proc/PID/exe (strips "(deleted)" after a
	// rebuild); Darwin falls back to ps -o comm= — weaker, but still catches
	// PID reuse by an unrelated process.
	if mismatch, err := shimPIDBinaryMismatch(state.ShimPID, m.naozhiBin); err == nil && mismatch {
		sendSIGUSR2(state.ShimPID) //nolint:errcheck
		RemoveStateFile(stateFile)
		return nil, fmt.Errorf("shim PID %d binary mismatch", state.ShimPID)
	} else if err != nil {
		slog.Warn("binary identity check skipped", "pid", state.ShimPID, "err", err)
	}

	// Validate socket path matches expected path exactly (prevents path injection)
	expectedSocket := SocketPath(keyHash)
	if state.Socket != expectedSocket {
		return nil, fmt.Errorf("socket path mismatch: got %s, expected %s", state.Socket, expectedSocket)
	}

	tokenRaw, err := base64.StdEncoding.DecodeString(state.AuthToken)
	if err != nil {
		return nil, fmt.Errorf("decode token: %w", err)
	}

	handle, err := m.connect(state.Socket, tokenRaw, lastSeq)
	if err != nil {
		return nil, err
	}
	handle.State = state

	m.mu.Lock()
	// Same invariant as StartShim: close a raced-in prior handle, never leak it.
	oldHandle := m.shims[key]
	m.shims[key] = handle
	m.mu.Unlock()
	if oldHandle != nil {
		oldHandle.Close()
	}

	return handle, nil
}

// connect establishes an authenticated connection to a shim socket.
func (m *Manager) connect(socketPath string, token []byte, lastSeq int64) (*ShimHandle, error) {
	conn, err := net.DialTimeout("unix", socketPath, 10*time.Second)
	if err != nil {
		// Include the socket path so operators can check it straight from the log.
		return nil, fmt.Errorf("dial shim at %s: %w", socketPath, err)
	}

	reader := bufio.NewReaderSize(conn, 256*1024) // 256KB buffer (bufio grows as needed for large lines)
	writer := bufio.NewWriter(conn)

	attach := ClientMsg{
		Type:  "attach",
		Token: base64.StdEncoding.EncodeToString(token),
		Seq:   lastSeq,
	}
	data, _ := json.Marshal(attach)
	// If SetWriteDeadline fails (peer closed between Dial and here) bail with the
	// real cause rather than letting Flush block without a deadline.
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set attach write deadline: %w", err)
	}
	writer.Write(data)         //nolint:errcheck
	writer.Write([]byte{'\n'}) //nolint:errcheck
	if err := writer.Flush(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write attach: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})

	// Read hello byte-by-byte through the same bufio (so later reads share its
	// state) with a 64 KB hard cap: bufio.ReadBytes has no upper bound and a
	// malicious shim could force unbounded buffering before authentication.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set hello read deadline: %w", err)
	}
	const maxHelloBytes = 64 * 1024
	// 1 KB initial cap fits a realistic hello and keeps the loop O(n).
	helloLine := make([]byte, 0, 1024)
	for len(helloLine) < maxHelloBytes {
		b, err := reader.ReadByte()
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("read hello: %w", err)
		}
		helloLine = append(helloLine, b)
		if b == '\n' {
			break
		}
	}
	if len(helloLine) == 0 || helloLine[len(helloLine)-1] != '\n' {
		conn.Close()
		return nil, fmt.Errorf("hello exceeds %d-byte cap without newline", maxHelloBytes)
	}
	conn.SetReadDeadline(time.Time{}) //nolint:errcheck

	var hello ServerMsg
	if err := json.Unmarshal(helloLine, &hello); err != nil {
		conn.Close()
		return nil, fmt.Errorf("parse hello: %w", err)
	}
	if hello.Type == "auth_failed" {
		conn.Close()
		return nil, fmt.Errorf("shim auth failed: %s", osutil.SanitizeForLog(hello.Msg, 128))
	}
	if hello.Type != "hello" {
		conn.Close()
		return nil, fmt.Errorf("unexpected message type: %s", osutil.SanitizeForLog(hello.Type, 64))
	}
	// Reject hellos outside [MinSupportedProtocolVersion, ProtocolVersion] so
	// deploy skew fails at attach instead of as a JSON parse error mid-session.
	// Pre-versioning shims send ProtocolVersion=0; treat 0 as v1 (#427).
	helloVer := hello.ProtocolVersion
	if helloVer == 0 {
		helloVer = 1
	}
	if helloVer < MinSupportedProtocolVersion || helloVer > ProtocolVersion {
		conn.Close()
		return nil, fmt.Errorf("shim protocol_version %d outside supported [%d,%d]; check naozhi/shim binary skew",
			helloVer, MinSupportedProtocolVersion, ProtocolVersion)
	}

	return &ShimHandle{
		Conn:       conn,
		Reader:     reader,
		Writer:     writer,
		Token:      token,
		Hello:      hello,
		ClientDone: make(chan struct{}),
	}, nil
}
