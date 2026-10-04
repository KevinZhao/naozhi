package shim

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// retireFake is a shim stand-in bound at SocketPath(KeyHash(key)). It answers
// every attach with hello (cli_alive as configured), replay_done and, for a
// dead CLI, cli_exited, then records the client's messages. On shutdown it
// closes its listener (which unlinks the socket) unless keepBound is set.
type retireFake struct {
	path      string
	ln        net.Listener
	token     []byte
	cliAlive  *bool
	keepBound bool
	// serveOnce answers only the first connection and leaves later ones
	// unanswered, like a shim whose reattach window was already taken.
	serveOnce bool

	accepted int // accept-loop only
	mu       sync.Mutex
	received []ClientMsg
	conns    sync.WaitGroup
}

// newRetireFake points SocketPath at a short temp dir and listens on key's
// socket. Not parallel-safe: it sets XDG_RUNTIME_DIR.
func newRetireFake(t *testing.T, key string, cliAlive *bool) *retireFake {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", shortSocketDir(t))
	path := SocketPath(KeyHash(key))
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	f := &retireFake{path: path, ln: ln, token: []byte("retire-token"), cliAlive: cliAlive}
	t.Cleanup(func() {
		_ = ln.Close()
		f.conns.Wait()
		_ = os.Remove(path)
	})
	return f
}

func (f *retireFake) serve() {
	f.conns.Add(1) // the accept loop itself, so per-conn Adds never start from zero
	go func() {
		defer f.conns.Done()
		for {
			conn, err := f.ln.Accept()
			if err != nil {
				return
			}
			f.conns.Add(1)
			served := f.accepted
			f.accepted++
			go func() {
				defer f.conns.Done()
				defer conn.Close()
				if f.serveOnce && served > 0 {
					_, _ = io.Copy(io.Discard, conn)
					return
				}
				f.handle(conn)
			}()
		}
	}()
}

func (f *retireFake) handle(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return // the pre-bind liveness dial connects and hangs up
	}
	var attach ClientMsg
	if json.Unmarshal(line, &attach) != nil {
		return
	}
	write := func(m ServerMsg) {
		data, _ := m.MarshalLine()
		_, _ = conn.Write(data)
	}
	if tok, err := base64.StdEncoding.DecodeString(attach.Token); err != nil || string(tok) != string(f.token) {
		write(ServerMsg{Type: "auth_failed", Msg: "invalid token"})
		return
	}
	write(ServerMsg{Type: "hello", ShimPID: os.Getpid(), CLIAlive: f.cliAlive, ProtocolVersion: ProtocolVersion})
	write(ServerMsg{Type: "replay_done"})
	if f.cliAlive != nil && !*f.cliAlive {
		write(ServerMsg{Type: "cli_exited", Code: intPtr(1)})
	}
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var msg ClientMsg
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		f.mu.Lock()
		f.received = append(f.received, msg)
		f.mu.Unlock()
		if msg.Type == "shutdown" {
			if !f.keepBound {
				_ = f.ln.Close()
			}
			return
		}
	}
}

// messages waits for every accepted conn to end, then returns what was sent.
func (f *retireFake) messages() []ClientMsg {
	_ = f.ln.Close()
	f.conns.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ClientMsg(nil), f.received...)
}

// writeRetireState records the fake as key's shim. ShimPID defaults to this
// test process, whose binary is the manager's naozhiBin.
func writeRetireState(t *testing.T, m *Manager, key string, f *retireFake, token []byte, pid int) {
	t.Helper()
	if pid == 0 {
		pid = os.Getpid()
	}
	st := State{
		ShimPID:   pid,
		Socket:    f.path,
		AuthToken: base64.StdEncoding.EncodeToString(token),
		Key:       key,
	}
	if err := WriteStateFile(StateFilePath(m.stateDir, KeyHash(key)), st); err != nil {
		t.Fatalf("WriteStateFile: %v", err)
	}
}

func socketExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestRetireDeadShim_ShutsDownShimWhoseCLIIsDead(t *testing.T) {
	const key = "feishu:direct:retire-dead:general"
	f := newRetireFake(t, key, boolPtr(false))
	f.serve()
	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
	writeRetireState(t, m, key, f, f.token, 0)

	retired, err := m.RetireDeadShim(context.Background(), key)
	if err != nil || !retired {
		t.Fatalf("RetireDeadShim = (%v, %v), want (true, nil)", retired, err)
	}
	if socketExists(f.path) {
		t.Error("socket still present after a successful retire")
	}
	msgs := f.messages()
	if len(msgs) != 1 || msgs[0].Type != "shutdown" {
		t.Errorf("fake received %+v, want exactly one shutdown", msgs)
	}
	m.mu.Lock()
	n := len(m.shims)
	m.mu.Unlock()
	if n != 0 {
		t.Errorf("m.shims has %d entries after retire, want 0 (the probe is never registered)", n)
	}
}

// A shim whose CLI is alive, or that does not report it, must only be probed:
// no shutdown, and the handle a live Process holds in m.shims stays open.
func TestRetireDeadShim_LeavesShimWithLiveOrUnknownCLI(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alive *bool
	}{
		{"alive", boolPtr(true)},
		{"unreported", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const key = "feishu:direct:retire-live:general"
			f := newRetireFake(t, key, tc.alive)
			f.serve()
			m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
			writeRetireState(t, m, key, f, f.token, 0)
			live, peer := net.Pipe()
			defer live.Close()
			defer peer.Close()
			existing := &ShimHandle{Conn: live, ClientDone: make(chan struct{})}
			m.shims[key] = existing

			retired, err := m.RetireDeadShim(context.Background(), key)
			if err != nil || retired {
				t.Fatalf("RetireDeadShim = (%v, %v), want (false, nil)", retired, err)
			}
			if !socketExists(f.path) {
				t.Error("socket removed for a shim whose CLI was not reported dead")
			}
			if msgs := f.messages(); len(msgs) != 0 {
				t.Errorf("fake received %+v, want nothing after the hello", msgs)
			}
			select {
			case <-existing.ClientDone:
				t.Error("the registered handle was closed by the probe")
			default:
			}
			if m.shims[key] != existing {
				t.Error("m.shims[key] was replaced by the probe")
			}
		})
	}
}

func TestRetireDeadShim_AuthFailureSendsNothing(t *testing.T) {
	const key = "feishu:direct:retire-auth:general"
	f := newRetireFake(t, key, boolPtr(false))
	f.serve()
	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
	writeRetireState(t, m, key, f, []byte("stale-token"), 0)

	retired, err := m.RetireDeadShim(context.Background(), key)
	if retired || err == nil || !strings.Contains(err.Error(), "auth failed") {
		t.Fatalf("RetireDeadShim = (%v, %v), want (false, auth failed)", retired, err)
	}
	if msgs := f.messages(); len(msgs) != 0 {
		t.Errorf("fake received %+v after auth_failed, want nothing", msgs)
	}
}

func TestRetireDeadShim_NoStateFile(t *testing.T) {
	const key = "feishu:direct:retire-nostate:general"
	f := newRetireFake(t, key, boolPtr(false))
	f.serve()
	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})

	start := time.Now()
	retired, err := m.RetireDeadShim(context.Background(), key)
	if retired || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("RetireDeadShim = (%v, %v), want (false, ErrNotExist)", retired, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("RetireDeadShim took %v without a state file, want an immediate failure", d)
	}
	if msgs := f.messages(); len(msgs) != 0 {
		t.Errorf("fake received %+v without a state file, want nothing", msgs)
	}
}

func TestRetireDeadShim_SocketNeverUnlinked(t *testing.T) {
	const key = "feishu:direct:retire-stuck:general"
	f := newRetireFake(t, key, boolPtr(false))
	f.keepBound = true
	f.serve()
	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
	writeRetireState(t, m, key, f, f.token, 0)

	start := time.Now()
	retired, err := m.RetireDeadShim(context.Background(), key)
	if retired || err == nil || !strings.Contains(err.Error(), "still bound") {
		t.Fatalf("RetireDeadShim = (%v, %v), want (false, still bound)", retired, err)
	}
	if d := time.Since(start); d < retireSocketWait {
		t.Errorf("gave up after %v, want the full %v socket wait", d, retireSocketWait)
	}
}

func TestPrepareSocketForSpawn(t *testing.T) {
	t.Run("dead CLI is retired", func(t *testing.T) {
		const key = "feishu:direct:prepare-dead:general"
		f := newRetireFake(t, key, boolPtr(false))
		f.serve()
		m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
		writeRetireState(t, m, key, f, f.token, 0)

		if err := m.prepareSocketForSpawn(context.Background(), key, f.path); err != nil {
			t.Fatalf("prepareSocketForSpawn: %v, want nil after retiring the dead-CLI shim", err)
		}
		if socketExists(f.path) {
			t.Error("socket still present, the new shim could not bind")
		}
	})
	// The retire probe has to be the first connection: a bare liveness dial
	// ahead of it would take the only one such a shim serves.
	t.Run("dead CLI serving one connection is retired", func(t *testing.T) {
		const key = "feishu:direct:prepare-once:general"
		f := newRetireFake(t, key, boolPtr(false))
		f.serveOnce = true
		f.serve()
		m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
		writeRetireState(t, m, key, f, f.token, 0)

		if err := m.prepareSocketForSpawn(context.Background(), key, f.path); err != nil {
			t.Fatalf("prepareSocketForSpawn: %v, want nil after retiring the dead-CLI shim", err)
		}
	})
	t.Run("live CLI keeps the refusal", func(t *testing.T) {
		const key = "feishu:direct:prepare-live:general"
		f := newRetireFake(t, key, boolPtr(true))
		f.serve()
		m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
		writeRetireState(t, m, key, f, f.token, 0)

		err := m.prepareSocketForSpawn(context.Background(), key, f.path)
		if err == nil || !strings.Contains(err.Error(), "refusing to clobber") {
			t.Fatalf("prepareSocketForSpawn = %v, want the refusing-to-clobber error", err)
		}
		if !socketExists(f.path) {
			t.Error("a live shim's socket was unlinked")
		}
	})
}

// TestRetirePIDHolderHelper is not a test: run from a copy of the test binary
// with retirePIDHolderEnv set, it parks so that copy can be a shim PID.
func TestRetirePIDHolderHelper(t *testing.T) {
	if os.Getenv(retirePIDHolderEnv) != "1" {
		t.Skip("helper process only")
	}
	<-time.After(time.Minute) // bounded, in case the parent never kills it
	os.Exit(0)
}

const retirePIDHolderEnv = "NAOZHI_TEST_RETIRE_PID_HOLDER"

// StartShimWithBackend runs the retire before it spawns. naozhiBin is a copy
// of the test binary: one running copy is the recorded shim PID (so the
// binary-identity check passes), then the copy is made non-executable so the
// spawn itself fails at cmd.Start, after the pre-bind check.
func TestStartShimWithBackend_RetiresDeadCLIShimBeforeSpawn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets and process identity checks")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "naozhi-fake")
	if err := os.WriteFile(bin, data, 0o700); err != nil {
		t.Fatal(err)
	}
	holder := exec.Command(bin, "-test.run=^TestRetirePIDHolderHelper$")
	holder.Env = append(os.Environ(), retirePIDHolderEnv+"=1")
	if err := holder.Start(); err != nil {
		t.Fatalf("start pid holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	if err := os.Chmod(bin, 0o600); err != nil {
		t.Fatal(err)
	}

	const key = "feishu:direct:startshim-retire:general"
	f := newRetireFake(t, key, boolPtr(false))
	f.serve()
	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir(), CLIPath: "/bin/true"})
	m.naozhiBin = bin
	writeRetireState(t, m, key, f, f.token, holder.Process.Pid)

	_, err = m.StartShimWithBackend(context.Background(), key, "", "", nil, t.TempDir(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "start shim") {
		t.Fatalf("StartShimWithBackend err = %v, want the cmd.Start failure past the pre-bind check", err)
	}
	if msgs := f.messages(); len(msgs) != 1 || msgs[0].Type != "shutdown" {
		t.Errorf("fake received %+v, want exactly one shutdown", msgs)
	}
}

// TestRetireDeadShim_RealShimServerAfterCLIExit drives the retire against the
// real shim server code in its post-exit reattach window (Run's accept loop,
// waitForReattach and teardown re-created in-process): the shim must exit and
// unlink its socket, also when a reattaching client already took the first
// window.
func TestRetireDeadShim_RealShimServerAfterCLIExit(t *testing.T) {
	for _, reattached := range []bool{false, true} {
		t.Run(map[bool]string{false: "first client", true: "after a reattach"}[reattached], func(t *testing.T) {
			const key = "feishu:direct:retire-real:general"
			t.Setenv("XDG_RUNTIME_DIR", shortSocketDir(t))
			socketPath := SocketPath(KeyHash(key))
			m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
			stateFile := StateFilePath(m.stateDir, KeyHash(key))

			tokenRaw, tokenB64, err := GenerateToken()
			if err != nil {
				t.Fatal(err)
			}
			cli, err := startCLI("sh", []string{"-c", "exit 1"}, t.TempDir())
			if err != nil {
				t.Fatalf("startCLI: %v", err)
			}
			cli.wait() //nolint:errcheck
			ln, err := net.Listen("unix", socketPath)
			if err != nil {
				t.Fatal(err)
			}
			s := &shimServer{
				cli:       cli,
				listener:  ln,
				buffer:    NewRingBuffer(100, 1024*1024),
				tokenRaw:  tokenRaw,
				stateFile: stateFile,
				state:     State{ShimPID: os.Getpid(), Key: key, Socket: socketPath, AuthToken: tokenB64},
				done:      make(chan struct{}),
				watchdog:  NewWatchdog(30*time.Second, nil),
			}
			s.saveStateCLIDead()

			var handlers sync.WaitGroup
			acceptCh := make(chan net.Conn)
			go func() {
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					select {
					case acceptCh <- conn:
					case <-s.done:
						conn.Close()
						return
					}
				}
			}()
			spawn := func(conn net.Conn) {
				handlers.Add(1)
				go func() { defer handlers.Done(); s.handleClient(conn, time.Minute) }()
			}
			runDone := make(chan struct{})
			go func() {
				defer close(runDone)
				s.waitForReattach(acceptCh, spawn, "cli exit")
				RemoveStateFile(stateFile)
				_ = os.Remove(socketPath)
				_ = ln.Close()
			}()
			t.Cleanup(func() {
				s.initiateShutdown()
				<-runDone
				handlers.Wait()
			})

			if reattached {
				h, err := m.connect(context.Background(), socketPath, tokenRaw, 0)
				if err != nil {
					t.Fatalf("reattach: %v", err)
				}
				if _, err := h.DrainReplay(context.Background()); err != nil {
					t.Fatalf("reattach drain: %v", err)
				}
				h.Close()
			}

			if err := m.prepareSocketForSpawn(context.Background(), key, socketPath); err != nil {
				t.Fatalf("prepareSocketForSpawn: %v, want the dead-CLI shim retired", err)
			}
			select {
			case <-runDone:
			case <-time.After(5 * time.Second):
				t.Fatal("shim still in its reattach window after the retire")
			}
		})
	}
}
