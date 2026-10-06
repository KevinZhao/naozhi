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
	// stderrDone is closed by readStderr at stderr EOF.
	stderrDone chan struct{}
	exited     chan struct{}
	exitCode   int
	exitOnce   sync.Once
	killOnce   sync.Once
}

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
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, fmt.Errorf("start: %w", err)
	}

	return &cliProc{
		cmd:        cmd,
		stdin:      stdin,
		stdout:     bufio.NewReaderSize(stdout, 64*1024),
		stderrR:    stderrPipe,
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

func (c *cliProc) wait() {
	c.exitOnce.Do(func() {
		_ = c.cmd.Wait()
		if c.cmd.ProcessState != nil {
			c.exitCode = c.cmd.ProcessState.ExitCode()
		}
		close(c.exited)
	})
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
