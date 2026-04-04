package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"syscall"
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

// Sentinel errors for watchdog timeouts.
var (
	ErrNoOutputTimeout = errors.New("no output timeout")
	ErrTotalTimeout    = errors.New("total timeout")
)

// processCloseTimeout is a var (not const) so tests can override it.
var processCloseTimeout = 5 * time.Second

func (s ProcessState) String() string {
	switch s {
	case StateSpawning:
		return "running" // spawning is transient; visible as running
	case StateReady:
		return "ready"
	case StateRunning:
		return "running"
	case StateDead:
		return "suspended" // process exited; session may be resumable
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

	eventCh    chan Event
	done       chan struct{}
	stderrDone chan struct{} // closed when stderr goroutine exits
	killCh     chan struct{} // closed by Kill() to unblock readLoop's channel send
	killOnce   sync.Once
	waitOnce   sync.Once // ensures cmd.Wait() is called exactly once

	noOutputTimeout time.Duration
	totalTimeout    time.Duration

	eventLog  *EventLog
	totalCost float64
}

// newProcess starts a CLI process with the given args.
func newProcess(ctx context.Context, cliPath string, args []string, cwd string, noOutputTimeout, totalTimeout time.Duration, proto Protocol) (*Process, error) {
	slog.Info("spawning cli process", "cli", cliPath, "protocol", proto.Name())
	slog.Debug("cli process details", "args", args, "cwd", cwd)

	cmd := exec.CommandContext(ctx, cliPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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

	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
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
		stderrDone:      stderrDone,
		killCh:          make(chan struct{}),
		noOutputTimeout: noOutputTimeout,
		totalTimeout:    totalTimeout,
		eventLog:        NewEventLog(0),
	}

	p.scanner.Buffer(make([]byte, 64*1024), maxScannerBufBytes)

	return p, nil
}

// startReadLoop begins the stdout reader goroutine. Called after protocol Init.
func (p *Process) startReadLoop() {
	p.mu.Lock()
	p.State = StateReady
	p.mu.Unlock()
	go p.readLoop()
}

// readLoop reads stdout NDJSON lines and sends parsed events to eventCh.
func (p *Process) readLoop() {
	// close(p.done) must run before close(p.eventCh) so that Alive() returns
	// false by the time any consumer of eventCh (e.g. Send) can observe the
	// channel closing. Defers run LIFO: eventCh registered first runs last.
	defer close(p.eventCh)
	defer close(p.done)
	defer p.eventLog.CloseSubscribers()

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
			p.mu.Lock()
			p.State = StateDead
			p.mu.Unlock()
			return
		}
	}
	if err := p.scanner.Err(); err != nil {
		slog.Warn("readLoop: scanner error", "err", err)
	} else {
		slog.Info("readLoop: stdout EOF, process exiting")
	}

	// Reap the child process to collect exit status.
	// waitOnce must be called before reading ProcessState to establish
	// a happens-before relationship (ProcessState is written by Wait).
	if p.cmd != nil {
		p.waitOnce.Do(func() { _ = p.cmd.Wait() })
	}
	if p.cmd != nil && p.cmd.ProcessState != nil {
		slog.Info("readLoop: process exit", "exit_code", p.cmd.ProcessState.ExitCode(), "pid", p.cmd.Process.Pid)
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

	// Log user message before sending
	userEntry := EventEntry{
		Time:    time.Now().UnixMilli(),
		Type:    "user",
		Summary: TruncateRunes(text, 120),
		Detail:  TruncateRunes(text, 2000),
	}
	if len(images) > 0 {
		userEntry.Summary += fmt.Sprintf(" [+%d image(s)]", len(images))
	}
	p.eventLog.Append(userEntry)

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
			// Stop the process: readLoop will close eventCh once it exits,
			// preventing stale events from being seen by a future Send() call.
			p.Kill()
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

			// Capture session ID from first init event; skip logging subsequent inits
			if ev.Type == "system" && ev.SubType == "init" {
				if p.SessionID == "" {
					p.SessionID = ev.SessionID
					p.logEvent(ev)
				}
				continue
			}

			p.logEvent(ev)

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
			return nil, fmt.Errorf("%w (%s)", ErrNoOutputTimeout, noOutputDur)
		case <-totalTimer.C:
			slog.Error("watchdog: total timeout", "timeout", totalDur)
			p.Kill()
			return nil, fmt.Errorf("%w (%s)", ErrTotalTimeout, totalDur)
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

// Interrupt sends SIGINT to the CLI process group to cancel the current turn.
// Uses negative PID to signal the entire process group (created via Setpgid).
// The process stays alive and can accept new messages after the interrupt.
func (p *Process) Interrupt() {
	if p.cmd.Process != nil {
		if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGINT); err != nil {
			slog.Warn("interrupt signal failed", "pid", p.cmd.Process.Pid, "err", err)
		}
	}
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
	timer := time.NewTimer(processCloseTimeout)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		slog.Warn("process close timeout, force killing", "pid", p.PID())
		p.Kill()
		return
	}
	// Wait for stderr goroutine to exit
	if p.stderrDone != nil {
		<-p.stderrDone
	}
	// Reap the child process to avoid zombies
	p.waitOnce.Do(func() { p.cmd.Wait() })
}

// logEvent converts an Event to an EventEntry and appends it to the event log.
func (p *Process) logEvent(ev Event) {
	entry := EventEntry{Time: time.Now().UnixMilli()}

	switch ev.Type {
	case "system":
		entry.Type = "system"
		entry.Summary = ev.SubType
		if ev.SubType == "init" {
			return
		}
		// Skip noisy system events that add no value in the dashboard
		switch ev.SubType {
		case "task_progress", "task_started", "task_notification",
			"stop_hook_summary", "turn_duration", "hook_started", "hook_response":
			return
		}
	case "assistant":
		if ev.Message == nil {
			return
		}
		for _, block := range ev.Message.Content {
			switch block.Type {
			case "thinking":
				entry.Type = "thinking"
				entry.Summary = TruncateRunes(block.Text, 120)
				entry.Detail = TruncateRunes(block.Text, 2000)
			case "tool_use":
				entry.Type = "tool_use"
				entry.Summary = block.Name
				entry.Tool = block.Name
				entry.Detail = formatToolDetail(block)
				if block.Name == "Agent" {
					inp := parseAgentInput(block.Input)
					entry.Type = "agent"
					entry.Subagent = inp.label()
					entry.Summary = TruncateRunes(inp.Description, 120)
					entry.Background = inp.RunInBackground
				}
			case "text":
				entry.Type = "text"
				entry.Summary = TruncateRunes(block.Text, 120)
				entry.Detail = TruncateRunes(block.Text, 16000)
			default:
				continue
			}
			p.eventLog.Append(entry)
			return
		}
		return
	case "result":
		entry.Type = "result"
		entry.Summary = TruncateRunes(ev.Result, 200)
		entry.Detail = TruncateRunes(ev.Result, 16000)
		entry.Cost = ev.CostUSD
		p.mu.Lock()
		p.totalCost = ev.CostUSD
		p.mu.Unlock()
	default:
		return
	}

	p.eventLog.Append(entry)
}

// agentInput holds the parsed fields from an Agent tool call input.
type agentInput struct {
	SubagentType    string `json:"subagent_type"`
	Name            string `json:"name"`
	TeamName        string `json:"team_name"`
	Description     string `json:"description"`
	RunInBackground bool   `json:"run_in_background"`
}

// parseAgentInput parses Agent tool input JSON. Returns zero-valued struct on error.
func parseAgentInput(input json.RawMessage) agentInput {
	if len(input) == 0 {
		return agentInput{}
	}
	var inp agentInput
	json.Unmarshal(input, &inp) //nolint:errcheck // zero-valued fields are safe defaults
	return inp
}

// label returns the best display label for an Agent call: subagent_type > name > team_name > "".
func (a agentInput) label() string {
	if a.SubagentType != "" {
		return a.SubagentType
	}
	if a.Name != "" {
		return a.Name
	}
	return a.TeamName
}

// formatToolDetail returns a human-readable detail string for tool_use events.
func formatToolDetail(block ContentBlock) string {
	if len(block.Input) == 0 {
		return block.Name
	}
	return FormatToolInput(block.Name, block.Input)
}

// getStr extracts a string value for the given key from a JSON object map.
func getStr(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok || len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// shortPath abbreviates a file path by replacing the home directory with ~.
func shortPath(p string) string {
	const homePrefix = "/home/"
	if i := strings.Index(p, homePrefix); i >= 0 {
		rest := p[i+len(homePrefix):]
		if j := strings.Index(rest, "/"); j >= 0 {
			return "~" + rest[j:]
		}
	}
	if len(p) > 50 {
		return "..." + p[len(p)-47:]
	}
	return p
}

// FormatToolInput extracts a human-readable summary from a tool's JSON input.
// Shared by live event logging and JSONL history loading.
func FormatToolInput(toolName string, input json.RawMessage) string {
	var inp map[string]json.RawMessage
	if json.Unmarshal(input, &inp) != nil {
		return toolName + ": " + TruncateRunes(string(input), 300)
	}

	switch toolName {
	case "Read":
		return toolName + " " + shortPath(getStr(inp, "file_path"))
	case "Write":
		return toolName + " " + shortPath(getStr(inp, "file_path"))
	case "Edit":
		return toolName + " " + shortPath(getStr(inp, "file_path"))
	case "Glob":
		return toolName + " " + getStr(inp, "pattern")
	case "Grep":
		s := toolName + " " + getStr(inp, "pattern")
		if path := getStr(inp, "path"); path != "" {
			s += " in " + shortPath(path)
		}
		return s
	case "Bash":
		if desc := getStr(inp, "description"); desc != "" {
			return toolName + " " + desc
		}
		return toolName + " " + TruncateRunes(getStr(inp, "command"), 80)
	case "Agent":
		return toolName + " " + TruncateRunes(getStr(inp, "description"), 60)
	default:
		for _, key := range []string{"description", "file_path", "path", "command", "pattern", "prompt"} {
			if v := getStr(inp, key); v != "" {
				return toolName + " " + TruncateRunes(v, 80)
			}
		}
		return toolName + ": " + TruncateRunes(string(input), 300)
	}
}

// GetState returns the current process state.
func (p *Process) GetState() ProcessState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.State
}

// GetSessionID returns the session ID in a thread-safe manner.
func (p *Process) GetSessionID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.SessionID
}

// TotalCost returns the cumulative cost.
func (p *Process) TotalCost() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.totalCost
}

// ProtocolName returns the protocol name.
func (p *Process) ProtocolName() string {
	return p.protocol.Name()
}

// PID returns the OS process ID.
func (p *Process) PID() int {
	if p.cmd != nil && p.cmd.Process != nil {
		return p.cmd.Process.Pid
	}
	return 0
}

// InjectHistory pre-populates the event log with historical entries.
// Must be called before any Send() to avoid interleaving with live events.
func (p *Process) InjectHistory(entries []EventEntry) {
	for _, e := range entries {
		p.eventLog.Append(e)
	}
}

// EventEntries returns a copy of all event log entries.
func (p *Process) EventEntries() []EventEntry {
	return p.eventLog.Entries()
}

// EventEntriesSince returns event log entries after the given unix ms timestamp.
func (p *Process) EventEntriesSince(afterMS int64) []EventEntry {
	return p.eventLog.EntriesSince(afterMS)
}

// TurnAgents returns the sub-agent types spawned in the current turn.
func (p *Process) TurnAgents() []SubagentInfo {
	return p.eventLog.TurnAgents()
}

// SubscribeEvents returns a notification channel and unsubscribe function for the event log.
func (p *Process) SubscribeEvents() (<-chan struct{}, func()) {
	return p.eventLog.Subscribe()
}
