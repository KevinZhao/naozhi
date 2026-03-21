package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"
)

// ProcessState represents the lifecycle state of a claude CLI process.
type ProcessState int

const (
	StateSpawning ProcessState = iota
	StateReady
	StateRunning
	StateDead
)

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

// Process manages a long-lived claude CLI subprocess.
type Process struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	scanner   *bufio.Scanner
	SessionID string
	State     ProcessState
	mu        sync.Mutex

	// eventCh delivers parsed events from stdout reader goroutine
	eventCh chan Event
	// done is closed when the process exits
	done chan struct{}
}

// newProcess starts a claude CLI process with the given args.
func newProcess(ctx context.Context, cliPath string, args []string, cwd string) (*Process, error) {
	slog.Info("spawning cli process", "cli", cliPath, "args", args, "cwd", cwd)

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
	// Capture stderr for debugging
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, fmt.Errorf("start cli: %w", err)
	}

	// Drain stderr in background and log
	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			slog.Debug("claude stderr", "line", scanner.Text())
		}
	}()

	p := &Process{
		cmd:     cmd,
		stdin:   stdin,
		scanner: bufio.NewScanner(stdout),
		State:   StateSpawning,
		eventCh: make(chan Event, 64),
		done:    make(chan struct{}),
	}

	// Set scanner buffer for potentially large NDJSON lines
	p.scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	// Start stdout reader goroutine
	go p.readLoop()

	// Note: init event only arrives after the first message is sent to stdin.
	// waitInit is called inside Send() on the first call.

	return p, nil
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
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		// Skip hook events
		if ev.Type == "system" && (ev.SubType == "hook_started" || ev.SubType == "hook_response") {
			continue
		}
		p.eventCh <- ev
	}

	p.mu.Lock()
	p.State = StateDead
	p.mu.Unlock()
}

// waitInit blocks until the first system/init event arrives or timeout.
func (p *Process) waitInit(timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case ev, ok := <-p.eventCh:
			if !ok {
				return fmt.Errorf("process exited before init")
			}
			if ev.Type == "system" && ev.SubType == "init" {
				p.mu.Lock()
				p.SessionID = ev.SessionID
				p.State = StateReady
				p.mu.Unlock()
				return nil
			}
		case <-timer.C:
			return fmt.Errorf("timeout waiting for init event")
		}
	}
}

// EventCallback is called for each intermediate event during Send.
type EventCallback func(ev Event)

// Send writes a user message to stdin and reads events until result.
// The onEvent callback is called for intermediate events (thinking, tool_use).
func (p *Process) Send(ctx context.Context, text string, onEvent EventCallback) (*SendResult, error) {
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

	// Write message to stdin
	msg := NewUserMessage(text)
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal message: %w", err)
	}
	data = append(data, '\n')
	if _, err := p.stdin.Write(data); err != nil {
		return nil, fmt.Errorf("write stdin: %w", err)
	}

	// Read events until result
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ev, ok := <-p.eventCh:
			if !ok {
				return nil, fmt.Errorf("process exited during send")
			}

			// Capture session ID from init event (first message only)
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

// Kill forcefully terminates the process.
func (p *Process) Kill() {
	p.stdin.Close()
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	p.cmd.Wait()
}

// Close gracefully shuts down by closing stdin.
func (p *Process) Close() {
	p.stdin.Close()
	// Wait for process to exit (with timeout)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		p.Kill()
	}
}
