package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"
)

// ProcessState represents the lifecycle state of a CLI process.
type ProcessState int

const (
	StateSpawning ProcessState = iota
	StateReady
	StateRunning
	StateDead
)

const (
	DefaultNoOutputTimeout = 2 * time.Minute
	DefaultTotalTimeout    = 5 * time.Minute
	maxScannerBufBytes     = 1024 * 1024
)

// processCloseTimeout is a var (not const) so tests can override it.
var processCloseTimeout = 5 * time.Second

func (s ProcessState) String() string {
	switch s {
	case StateSpawning:
		return "spawning"
	case StateReady:
		return "ready"
	case StateRunning:
		return "running"
	case StateDead:
		return "dead"
	default:
		return "unknown"
	}
}

// Process manages a long-lived CLI subprocess.
type Process struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	scanner  *bufio.Scanner
	protocol Protocol

	SessionID string
	State     ProcessState
	mu        sync.Mutex

	eventCh  chan Event
	done     chan struct{}
	killCh   chan struct{} // closed by Kill() to unblock readLoop's channel send
	killOnce sync.Once
	waitOnce sync.Once // ensures cmd.Wait() is called exactly once

	noOutputTimeout time.Duration
	totalTimeout    time.Duration
}

// newProcess starts a CLI process with the given args.
func newProcess(ctx context.Context, cliPath string, args []string, cwd string, noOutputTimeout, totalTimeout time.Duration, proto Protocol) (*Process, error) {
	slog.Info("spawning cli process", "cli", cliPath, "protocol", proto.Name())
	slog.Debug("cli process details", "args", args, "cwd", cwd)

	cmd := exec.CommandContext(ctx, cliPath, args...)
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
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, fmt.Errorf("start cli: %w", err)
	}

	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			slog.Debug("cli stderr", "line", scanner.Text())
		}
	}()

	p := &Process{
		cmd:             cmd,
		stdin:           stdin,
		scanner:         bufio.NewScanner(stdout),
		protocol:        proto,
		State:           StateSpawning,
		eventCh:         make(chan Event, 64),
		done:            make(chan struct{}),
		killCh:          make(chan struct{}),
		noOutputTimeout: noOutputTimeout,
		totalTimeout:    totalTimeout,
	}

	p.scanner.Buffer(make([]byte, 0, maxScannerBufBytes), maxScannerBufBytes)

	return p, nil
}

// startReadLoop begins the stdout reader goroutine. Called after protocol Init.
func (p *Process) startReadLoop() {
	go p.readLoop()
}

// readLoop reads stdout NDJSON lines and sends parsed events to eventCh.
func (p *Process) readLoop() {
	defer close(p.done)
	defer close(p.eventCh)

	for p.scanner.Scan() {
		line := p.scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		ev, _, err := p.protocol.ReadEvent(line)
		if err != nil {
			slog.Debug("readLoop: skip unparseable event", "err", err)
			continue
		}
		if ev.Type == "" {
			continue
		}
		// Let protocol handle internal events (e.g., ACP permission auto-grant)
		if p.protocol.HandleEvent(p.stdin, ev) {
			continue
		}
		// Use select to avoid blocking forever if Kill() is called while eventCh is full
		select {
		case p.eventCh <- ev:
		case <-p.killCh:
			return
		}
	}

	p.mu.Lock()
	p.State = StateDead
	p.mu.Unlock()
}

// EventCallback is called for each intermediate event during Send.
type EventCallback func(ev Event)

// Send writes a user message to stdin and reads events until result.
func (p *Process) Send(ctx context.Context, text string, images []ImageData, onEvent EventCallback) (*SendResult, error) {
	p.mu.Lock()
	if p.State == StateRunning {
		p.mu.Unlock()
		return nil, fmt.Errorf("process busy (state=%s)", p.State)
	}
	p.State = StateRunning
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		if p.State == StateRunning {
			p.State = StateReady
		}
		p.mu.Unlock()
	}()

	if err := p.protocol.WriteMessage(p.stdin, text, images); err != nil {
		return nil, fmt.Errorf("write message: %w", err)
	}

	noOutputDur := p.noOutputTimeout
	if noOutputDur <= 0 {
		noOutputDur = DefaultNoOutputTimeout
	}
	totalDur := p.totalTimeout
	if totalDur <= 0 {
		totalDur = DefaultTotalTimeout
	}
	noOutputTimer := time.NewTimer(noOutputDur)
	defer noOutputTimer.Stop()
	totalTimer := time.NewTimer(totalDur)
	defer totalTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ev, ok := <-p.eventCh:
			if !ok {
				return nil, fmt.Errorf("process exited during send")
			}

			if !noOutputTimer.Stop() {
				select {
				case <-noOutputTimer.C:
				default:
				}
			}
			noOutputTimer.Reset(noOutputDur)

			// Capture session ID from init event
			if ev.Type == "system" && ev.SubType == "init" && p.SessionID == "" {
				p.SessionID = ev.SessionID
				slog.Info("session initialized", "session_id", ev.SessionID)
				continue
			}

			// Deliver intermediate events via callback
			if onEvent != nil && ev.Type == "assistant" && ev.Message != nil {
				for _, block := range ev.Message.Content {
					if block.Type == "thinking" || block.Type == "tool_use" {
						onEvent(ev)
						break
					}
				}
			}

			// Result means this turn is done
			if ev.Type == "result" {
				if p.SessionID == "" {
					p.SessionID = ev.SessionID
				}
				return &SendResult{
					Text:      ev.Result,
					SessionID: ev.SessionID,
					CostUSD:   ev.CostUSD,
				}, nil
			}
		case <-noOutputTimer.C:
			slog.Error("watchdog: no output timeout", "timeout", noOutputDur)
			p.Kill()
			return nil, fmt.Errorf("no output timeout (%s)", noOutputDur)
		case <-totalTimer.C:
			slog.Error("watchdog: total timeout", "timeout", totalDur)
			p.Kill()
			return nil, fmt.Errorf("total timeout (%s)", totalDur)
		}
	}
}

// Alive returns true if the process has not exited.
func (p *Process) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// IsRunning returns true if the process is currently processing a message.
func (p *Process) IsRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.State == StateRunning
}

// Kill forcefully terminates the process.
// Safe to call concurrently — all operations are guarded by Once.
func (p *Process) Kill() {
	p.killOnce.Do(func() {
		close(p.killCh)
		p.stdin.Close()
		if p.cmd.Process != nil {
			p.cmd.Process.Kill()
		}
	})
	p.waitOnce.Do(func() { p.cmd.Wait() })
}

// Close gracefully shuts down by closing stdin.
func (p *Process) Close() {
	p.stdin.Close()
	select {
	case <-p.done:
	case <-time.After(processCloseTimeout):
		p.Kill()
		return
	}
	// Reap the child process to avoid zombies
	p.waitOnce.Do(func() { p.cmd.Wait() })
}
