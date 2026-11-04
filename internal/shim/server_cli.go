package shim

// CLI child-process supervision: spawn, stdout/stderr pumps, session-id
// extraction and process teardown.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// maxStdoutLineBytes caps one CLI stdout line. A longer line is drained and
// skipped (logged + counted), never fatal: with a bufio.Scanner the cap ended
// the read loop while the CLI kept writing, so once the pipe filled the CLI
// blocked forever with both processes alive (#3578).
const maxStdoutLineBytes = 10 * 1024 * 1024

// stdoutOversizeTotal counts skipped over-cap stdout lines.
var stdoutOversizeTotal atomic.Int64

// readStdout reads CLI stdout and pushes lines to the ring buffer + client.
func (s *shimServer) readStdout() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("shim readStdout panic recovered",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	lineBuf := make([]byte, 0, 64*1024)
	for {
		line, oversize, err := readCappedLine(s.cli.stdout, lineBuf, maxStdoutLineBytes)
		lineBuf = line[:0]
		if oversize {
			n := stdoutOversizeTotal.Add(1)
			slog.Warn("CLI stdout line over cap; skipped", "cap_bytes", maxStdoutLineBytes, "skipped_total", n)
			s.watchdog.Reset() // the CLI is alive and talking, just too loudly
		}
		if err != nil && len(line) == 0 {
			break // EOF (or read error) with nothing left to forward
		}
		if oversize {
			continue
		}
		// A final unterminated line (err == io.EOF, len > 0) is forwarded
		// below; the loop ends on the next read.

		seq := s.buffer.Push(line) // Push makes its own copy for replay storage
		s.watchdog.Reset()

		s.tryExtractSessionID(line)

		// Skip the per-line frame marshal when no naozhi is attached (#2295);
		// Push/extract/Reset above still run. A client attaching mid-line
		// misses one frame, which replay backfills on attach.
		if !s.clientAttached.Load() {
			continue
		}

		// MarshalStdoutLine aliases `line` via unsafe.String; enqueueWrite hands
		// the result off before the next Scan(), so the alias never outlives
		// this iteration.
		if data, err := MarshalStdoutLine(seq, line); err == nil {
			s.enqueueWrite(data)
		}
		if err != nil {
			break
		}
	}

	// cmd.Wait closes the stderr pipe, so reaping before readStderr reaches
	// EOF can drop the CLI's final lines, which are the error message. The
	// wait is bounded: a grandchild holding the stderr fd must not stall it.
	s.cli.awaitStderrEOF(stderrDrainTimeout)
	s.cli.wait()
	slog.Info("CLI stdout EOF")
}

// stderrDrainTimeout bounds how long readStdout waits for stderr EOF after
// stdout EOF before reaping the CLI.
const stderrDrainTimeout = 500 * time.Millisecond

// cliTermGrace is how long terminate gives the CLI to exit on SIGTERM before
// the SIGKILL. A var so tests can shorten it.
var cliTermGrace = 2 * time.Second

// cliOrphanGrace is how long stdout may stay open after the CLI's direct
// child exited before watchChild ends its process group; a CLI that exits
// closes stdout at once. A var so tests can shorten it.
var cliOrphanGrace = 3 * time.Second

// tryExtractSessionID search keys, hoisted to avoid a per-line []byte alloc.
// snake = claude stream-json frames, camel = ACP/kiro frames.
var (
	sessionIDSnakeKey = []byte(`"session_id"`)
	sessionIDCamelKey = []byte(`"sessionId"`)
)

func (s *shimServer) tryExtractSessionID(line []byte) {
	// The ID is assigned once and never cleared; the latch skips the scan and
	// s.mu on the common already-known case.
	if s.sessionIDKnown.Load() {
		return
	}

	// Only init / result / ACP session/new frames carry the ID; two Contains
	// scans avoid a decoder alloc for the vast majority of lines.
	hasSnake := bytes.Contains(line, sessionIDSnakeKey)
	hasCamel := bytes.Contains(line, sessionIDCamelKey)
	if !hasSnake && !hasCamel {
		return
	}

	if hasSnake {
		var ev struct {
			Type      string `json:"type"`
			SubType   string `json:"subtype"`
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(line, &ev) == nil && ev.SessionID != "" {
			s.mu.Lock()
			if ev.Type == "system" && ev.SubType == "init" {
				s.state.SessionID = ev.SessionID
			}
			if ev.Type == "result" && s.state.SessionID == "" {
				s.state.SessionID = ev.SessionID
			}
			known := s.state.SessionID != ""
			s.mu.Unlock()
			if known {
				s.sessionIDKnown.Store(true)
			}
			return
		}
	}

	if hasCamel {
		// ACP / kiro: session/new response carries result.sessionId; later
		// session/update notifications echo it in params. First sighting wins.
		var ev struct {
			Result struct {
				SessionID string `json:"sessionId"`
			} `json:"result"`
			Params struct {
				SessionID string `json:"sessionId"`
			} `json:"params"`
		}
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		sid := ev.Result.SessionID
		if sid == "" {
			sid = ev.Params.SessionID
		}
		if sid == "" {
			return
		}
		s.mu.Lock()
		if s.state.SessionID == "" {
			s.state.SessionID = sid
		}
		known := s.state.SessionID != ""
		s.mu.Unlock()
		if known {
			s.sessionIDKnown.Store(true)
		}
	}
}

// readStderr reads CLI stderr into the stderr tail and forwards each line to
// the client. The tail is kept whether or not a client is attached, so a CLI
// that fails before naozhi attaches still reports why on cli_exited.
func (s *shimServer) readStderr() {
	if done := s.cli.stderrDone; done != nil {
		defer close(done)
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("shim readStderr panic recovered",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	scanner := bufio.NewScanner(s.cli.stderrR)
	scanner.Buffer(make([]byte, 4*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		s.stderrTail.Push(line)
		safeLine := osutil.SanitizeForLog(line, 512)
		slog.Debug("cli stderr", "line", safeLine)

		wsLine := line
		if len(line) > 64*1024 {
			wsLine = line[:64*1024] + "...[truncated]"
		}
		msg := ServerMsg{Type: "stderr", Line: wsLine}
		if data, err := msg.MarshalLine(); err == nil {
			s.enqueueWrite(data)
		}
	}
}

// --- CLI process management ---

type cliProc struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	stderrR io.ReadCloser
	// stdoutR is stdout's read end, closed once the CLI is reaped.
	stdoutR io.Closer
	// stderrDone is closed by readStderr at stderr EOF.
	stderrDone chan struct{}
	// exited is closed once stdout reached EOF (or the CLI was killed) and
	// the direct child was reaped.
	exited   chan struct{}
	exitCode int
	reapOnce sync.Once
	exitOnce sync.Once
	killOnce sync.Once
}

// startCLI starts the CLI with stdout and stderr on pipes of its own: Wait
// closes the pipes it creates, so reaping the direct child the moment it
// exits (watchChild) would drop output the CLI wrote last.
func startCLI(cliPath string, args []string, cwd string) (*cliProc, error) {
	cmd := exec.Command(cliPath, args...)
	setSetsid(cmd)
	if cwd != "" {
		cmd.Dir = cwd
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdin.Close()
		stdoutR.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	cmd.Stdout, cmd.Stderr = stdoutW, stderrW

	err = cmd.Start()
	// The child holds its own copies; ours would keep EOF from ever coming.
	stdoutW.Close()
	stderrW.Close()
	if err != nil {
		stdin.Close()
		stdoutR.Close()
		stderrR.Close()
		return nil, fmt.Errorf("start: %w", err)
	}

	return &cliProc{
		cmd:        cmd,
		stdin:      stdin,
		stdout:     bufio.NewReaderSize(stdoutR, 64*1024),
		stdoutR:    stdoutR,
		stderrR:    stderrR,
		stderrDone: make(chan struct{}),
		exited:     make(chan struct{}),
	}, nil
}

// awaitStderrEOF waits until readStderr has drained stderr or timeout elapses.
func (c *cliProc) awaitStderrEOF(timeout time.Duration) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-c.stderrDone:
	case <-t.C:
	}
}

func (c *cliProc) pid() int {
	if c.cmd.Process != nil {
		return c.cmd.Process.Pid
	}
	return 0
}

func (c *cliProc) alive() bool {
	select {
	case <-c.exited:
		return false
	default:
		return true
	}
}

// reap waits for the direct child to exit and records its exit code.
func (c *cliProc) reap() {
	c.reapOnce.Do(func() {
		_ = c.cmd.Wait()
		if c.cmd.ProcessState != nil {
			c.exitCode = c.cmd.ProcessState.ExitCode()
		}
	})
}

// wait reaps the CLI, closes its output pipes (unblocking a reader a
// descendant still holds them open for) and closes exited.
func (c *cliProc) wait() {
	c.exitOnce.Do(func() {
		c.reap()
		if c.stdoutR != nil {
			_ = c.stdoutR.Close()
		}
		if c.stderrR != nil {
			_ = c.stderrR.Close()
		}
		close(c.exited)
	})
}

// watchChild ends what the direct child leaves behind when it dies alone. A
// wrapper script that runs the real CLI as its child (~/.local/bin/claude)
// can be killed without it: the CLI keeps stdout open, readStdout never sees
// EOF and the session reads ready forever. Once the child has exited and
// stdout is still open after cliOrphanGrace, its process group, the CLI in
// it, is terminated as waitOrKill does, and exited follows with the child's
// exit code.
func (c *cliProc) watchChild() {
	c.reap()
	t := time.NewTimer(cliOrphanGrace)
	defer t.Stop()
	select {
	case <-c.exited:
		return
	case <-t.C:
	}
	slog.Warn("CLI child exited but its output is still open; ending its process group",
		"pid", c.pid(), "code", c.exitCode)
	c.terminate()
}

func (c *cliProc) interrupt() {
	if !c.alive() {
		return
	}
	if c.cmd.Process != nil {
		_ = sendProcGroupSIGINT(c.cmd.Process.Pid)
	}
}

func (c *cliProc) kill() {
	c.killOnce.Do(func() {
		_ = c.stdin.Close()
		if c.cmd.Process != nil {
			_ = sendProcGroupSIGKILL(c.cmd.Process.Pid)
		}
	})
	c.wait()
}

func (c *cliProc) closeStdin() {
	_ = c.stdin.Close()
}

func (c *cliProc) waitOrKill(timeout time.Duration) {
	c.closeStdin()
	// NewTimer + Stop rather than time.After so the fast path does not leave a
	// parked timer for the full timeout.
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-c.exited:
		return
	case <-t.C:
	}
	// A CLI mid-turn does not exit on EOF.
	c.terminate()
}

// terminate sends the CLI's process group SIGTERM, and SIGKILL if it is
// still there after cliTermGrace. SIGTERM lets the CLI end its tools'
// processes, which run in process groups of their own: the SIGKILL of the
// CLI's group alone left a stopped session's running command behind.
func (c *cliProc) terminate() {
	if c.cmd.Process != nil {
		_ = sendProcGroupSIGTERM(c.cmd.Process.Pid)
	}
	t := time.NewTimer(cliTermGrace)
	defer t.Stop()
	select {
	case <-c.exited:
	case <-t.C:
		c.kill()
	}
}

// readCappedLine reads one '\n'-terminated line into buf (reused across calls;
// the returned slice aliases it). A line longer than capBytes is drained to its
// terminator and reported as oversize with an empty line. err is io.EOF (or a
// read error) only when the stream ended; a final unterminated line is
// returned together with io.EOF.
func readCappedLine(r *bufio.Reader, buf []byte, capBytes int) (line []byte, oversize bool, err error) {
	line = buf[:0]
	for {
		chunk, rerr := r.ReadSlice('\n')
		if len(line)+len(chunk) > capBytes {
			// Drain the rest of this line unless this chunk already ended it.
			if rerr != nil && errors.Is(rerr, bufio.ErrBufferFull) {
				for {
					_, derr := r.ReadSlice('\n')
					if derr == nil {
						break
					}
					if !errors.Is(derr, bufio.ErrBufferFull) {
						return nil, true, derr
					}
				}
			} else if rerr != nil {
				return nil, true, rerr
			}
			return nil, true, nil
		}
		line = append(line, chunk...)
		switch {
		case rerr == nil:
			return line[:len(line)-1], false, nil // strip '\n'
		case errors.Is(rerr, bufio.ErrBufferFull):
			continue
		default:
			return line, false, rerr
		}
	}
}
