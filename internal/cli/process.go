package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
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
	maxScannerBufBytes     = 10 * 1024 * 1024
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

// shimMsg is a minimal struct for parsing shim protocol messages in readLoop.
type shimMsg struct {
	Type   string `json:"type"`
	Seq    int64  `json:"seq,omitempty"`
	Line   string `json:"line,omitempty"`
	Code   *int   `json:"code,omitempty"`
	Signal string `json:"signal,omitempty"`
}

// Process manages a CLI subprocess via a shim connection.
type Process struct {
	shimConn    net.Conn
	shimR       *bufio.Reader
	shimW       *bufio.Writer
	shimWMu     sync.Mutex
	stdinWriter *shimWriter // cached shimStdinWriter instance
	protocol    Protocol
	cliPID      int // CLI PID reported by shim hello

	SessionID string
	State     ProcessState
	mu        sync.Mutex

	eventCh  chan Event
	done     chan struct{}
	killCh   chan struct{} // closed by Kill() to unblock readLoop
	killOnce sync.Once

	noOutputTimeout time.Duration
	totalTimeout    time.Duration
	interrupted     atomic.Bool // set by Interrupt(), cleared by next Send()

	eventLog  *EventLog
	totalCost float64
	lastSeq   atomic.Int64  // last received shim seq, for reconnect
	pongRecv  chan struct{} // signaled by readLoop on pong receipt
}

// newShimProcess creates a Process connected to a shim.
// The caller must call startReadLoop() after protocol Init.
func newShimProcess(conn net.Conn, reader *bufio.Reader, writer *bufio.Writer,
	proto Protocol, cliPID int, noOutputTimeout, totalTimeout time.Duration) *Process {
	p := &Process{
		shimConn:        conn,
		shimR:           reader,
		shimW:           writer,
		protocol:        proto,
		cliPID:          cliPID,
		State:           StateSpawning,
		eventCh:         make(chan Event, 256),
		done:            make(chan struct{}),
		killCh:          make(chan struct{}),
		noOutputTimeout: noOutputTimeout,
		totalTimeout:    totalTimeout,
		eventLog:        NewEventLog(0),
		pongRecv:        make(chan struct{}, 1),
	}
	p.stdinWriter = &shimWriter{p: p}
	return p
}

// shimStdinWriter returns an io.Writer that sends data to CLI stdin via the shim.
// Returns the same instance each call to preserve any buffered partial lines.
// Always non-nil: initialized in newShimProcess to avoid lazy-init data races
// when readLoop and Send call this concurrently on the SpawnReconnect path.
func (p *Process) shimStdinWriter() io.Writer {
	return p.stdinWriter
}

// shimWriter wraps shim protocol write commands as an io.Writer.
// Thread-safe: readLoop (HandleEvent) and Send (WriteMessage) may call concurrently.
type shimWriter struct {
	p   *Process
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *shimWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf.Write(data)
	// Protocol.WriteMessage writes complete NDJSON lines ending with \n.
	// Buffer until we see a newline, then send each complete line as a shim write command.
	for {
		line, err := w.buf.ReadBytes('\n')
		if err != nil {
			// No newline yet — put the partial data back
			w.buf.Write(line)
			break
		}
		trimmed := bytes.TrimRight(line, "\n")
		if err := w.p.shimSend(shimClientMsg{Type: "write", Line: string(trimmed)}); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}

// shimClientMsg is the outgoing message format to the shim.
type shimClientMsg struct {
	Type  string `json:"type"`
	Line  string `json:"line,omitempty"`
	Token string `json:"token,omitempty"`
	Seq   int64  `json:"last_seq,omitempty"`
}

func (p *Process) shimSend(msg shimClientMsg) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	p.shimWMu.Lock()
	defer p.shimWMu.Unlock()
	if _, err := p.shimW.Write(data); err != nil {
		return err
	}
	if err := p.shimW.WriteByte('\n'); err != nil {
		return err
	}
	return p.shimW.Flush()
}

// startReadLoop begins the shim message reader goroutine and heartbeat.
func (p *Process) startReadLoop() {
	p.mu.Lock()
	p.State = StateReady
	p.mu.Unlock()
	go p.readLoop()
	go p.heartbeatLoop()
}

// readLoop reads NDJSON messages from the shim socket and dispatches events.
func (p *Process) readLoop() {
	defer close(p.eventCh)
	defer close(p.done)
	defer p.eventLog.CloseSubscribers()

	for {
		line, err := p.shimR.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				slog.Info("readLoop: shim connection closed")
			} else {
				slog.Warn("readLoop: shim read error", "err", err)
			}
			break
		}
		if len(line) > maxScannerBufBytes {
			slog.Warn("readLoop: oversized shim message, skipping", "size", len(line))
			continue
		}

		var msg shimMsg
		if err := json.Unmarshal(bytes.TrimSpace(line), &msg); err != nil {
			slog.Debug("readLoop: skip unparseable shim message", "err", err)
			continue
		}

		switch msg.Type {
		case "stdout":
			p.lastSeq.Store(msg.Seq)
			ev, _, err := p.protocol.ReadEvent([]byte(msg.Line))
			if err != nil {
				slog.Debug("readLoop: skip unparseable event", "err", err)
				continue
			}
			if ev.Type == "" {
				continue
			}
			if p.protocol.HandleEvent(p.shimStdinWriter(), ev) {
				continue
			}

			// Always log to EventLog so dashboard subscribers see events
			// even when no Send() is active (e.g., after service restart
			// reconnects to a shim that's mid-turn).
			p.logEvent(ev)

			select {
			case <-p.killCh:
				p.mu.Lock()
				p.State = StateDead
				p.mu.Unlock()
				return
			default:
			}

			// Deliver to Send() for result detection and callback delivery.
			// Non-blocking: if buffer is full (no active Send), the event
			// is already safely in EventLog for dashboard visibility.
			select {
			case p.eventCh <- ev:
			default:
			}

		case "stderr":
			slog.Debug("cli stderr", "line", msg.Line)

		case "cli_exited":
			code := 0
			if msg.Code != nil {
				code = *msg.Code
			}
			slog.Info("CLI exited via shim", "code", code)
			p.mu.Lock()
			p.State = StateDead
			p.mu.Unlock()
			return

		case "pong":
			// Signal heartbeat loop that shim is responsive
			select {
			case p.pongRecv <- struct{}{}:
			default:
			}

		case "error":
			slog.Warn("shim error", "msg", msg.Line)
		}
	}

	p.mu.Lock()
	p.State = StateDead
	p.mu.Unlock()
}

// heartbeatLoop sends periodic ping messages to the shim and kills the process
// if 3 consecutive pongs are missed (shim unresponsive or connection broken).
func (p *Process) heartbeatLoop() {
	const (
		interval  = 30 * time.Second
		maxMisses = 3
	)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	misses := 0
	pongTimer := time.NewTimer(interval / 2)
	pongTimer.Stop()
	defer pongTimer.Stop()
	for {
		select {
		case <-ticker.C:
			if err := p.shimSend(shimClientMsg{Type: "ping"}); err != nil {
				slog.Debug("heartbeat ping failed", "err", err)
				p.Kill()
				return
			}

			// Wait for pong within half the interval
			pongTimer.Reset(interval / 2)
			select {
			case <-p.pongRecv:
				pongTimer.Stop()
				misses = 0
			case <-pongTimer.C:
				misses++
				slog.Debug("heartbeat pong missed", "misses", misses)
				if misses >= maxMisses {
					slog.Warn("heartbeat: shim unresponsive, killing process", "misses", misses)
					p.Kill()
					return
				}
			case <-p.done:
				pongTimer.Stop()
				return
			}

		case <-p.done:
			return
		}
	}
}

// findResultSince checks EventLog for a result entry logged after afterMS.
// Used as fallback when eventCh may have dropped events due to full buffer.
func (p *Process) findResultSince(afterMS int64) *SendResult {
	entries := p.eventLog.EntriesSince(afterMS)
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type == "result" {
			return &SendResult{
				Text:      entries[i].Detail,
				SessionID: p.GetSessionID(),
				CostUSD:   entries[i].Cost,
			}
		}
	}
	return nil
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
		thumbs := make([]string, len(images))
		var wg sync.WaitGroup
		for i, img := range images {
			wg.Add(1)
			go func(i int, data []byte) {
				defer wg.Done()
				thumbs[i] = MakeThumbnail(data, 200)
			}(i, img.Data)
		}
		wg.Wait()
		filtered := thumbs[:0]
		for _, t := range thumbs {
			if t != "" {
				filtered = append(filtered, t)
			}
		}
		userEntry.Images = filtered
	}
	p.eventLog.Append(userEntry)

	// Drain stale events from a previous turn that completed while no Send()
	// was active (e.g., CLI was mid-turn when service restarted and reconnected
	// to shim). These events are already logged to EventLog by readLoop.
	//
	// When the previous turn was interrupted (SIGINT), the CLI may still be
	// producing the interrupted result. Wait briefly for it so it doesn't
	// pollute this turn's event stream.
	if err := p.drainStaleEvents(ctx); err != nil {
		return nil, err
	}

	// Record turn start time so we can check EventLog as fallback if eventCh
	// drops events (non-blocking send when buffer is full).
	turnStartMS := time.Now().UnixMilli()

	if err := p.protocol.WriteMessage(p.shimStdinWriter(), text, images); err != nil {
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
			// Context cancelled (shutdown or user interrupt).
			// Don't Kill the CLI — during graceful shutdown, router.Shutdown
			// calls Detach() to keep the shim alive for zero-downtime restart.
			// The readLoop will detect the disconnection and close eventCh,
			// causing the next iteration to hit the !ok branch and return.
			return nil, ctx.Err()
		case ev, ok := <-p.eventCh:
			if !ok {
				// eventCh closed — process exited. Check EventLog for a result
				// that readLoop already logged but wasn't delivered via eventCh
				// (e.g., non-blocking send dropped it, or it arrived just before
				// the channel closed).
				if sr := p.findResultSince(turnStartMS); sr != nil {
					return sr, nil
				}
				return nil, fmt.Errorf("process exited during send")
			}

			if !noOutputTimer.Stop() {
				select {
				case <-noOutputTimer.C:
				default:
				}
			}
			noOutputTimer.Reset(noOutputDur)

			// Capture session ID from first init event.
			// logEvent (called by readLoop) already skips init events.
			if ev.Type == "system" && ev.SubType == "init" {
				p.mu.Lock()
				if p.SessionID == "" {
					p.SessionID = ev.SessionID
				}
				p.mu.Unlock()
				continue
			}

			// Event is already logged to EventLog by readLoop.

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
				p.mu.Lock()
				if p.SessionID == "" {
					p.SessionID = ev.SessionID
				}
				p.mu.Unlock()
				return &SendResult{
					Text:      ev.Result,
					SessionID: ev.SessionID,
					CostUSD:   ev.CostUSD,
				}, nil
			}
		case <-noOutputTimer.C:
			// Before reporting timeout, check if result was logged to EventLog
			// but missed by eventCh (non-blocking send dropped it).
			if sr := p.findResultSince(turnStartMS); sr != nil {
				return sr, nil
			}
			slog.Error("watchdog: no output timeout", "timeout", noOutputDur)
			p.Kill()
			return nil, fmt.Errorf("%w (%s)", ErrNoOutputTimeout, noOutputDur)
		case <-totalTimer.C:
			if sr := p.findResultSince(turnStartMS); sr != nil {
				return sr, nil
			}
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

// Interrupt sends SIGINT to the CLI process via shim.
func (p *Process) Interrupt() {
	if !p.Alive() {
		return
	}
	p.interrupted.Store(true)
	if err := p.shimSend(shimClientMsg{Type: "interrupt"}); err != nil {
		slog.Warn("interrupt failed", "err", err)
	}
}

// drainStaleEvents clears residual events from previous turns.
// When the previous turn was interrupted (SIGINT), waits briefly for the
// interrupted result event so it doesn't pollute the next turn.
func (p *Process) drainStaleEvents(ctx context.Context) error {
	if p.interrupted.Swap(false) {
		// Only wait for the interrupted result if the CLI was actively
		// processing a turn. An idle process (State != Running) won't
		// produce a result event, so the settle timer would always expire
		// causing an unnecessary 500ms delay.
		if p.IsRunning() {
			slog.Debug("send: draining interrupted turn result")
			settle := time.NewTimer(500 * time.Millisecond)
			defer settle.Stop()
			for {
				select {
				case ev, ok := <-p.eventCh:
					if !ok || ev.Type == "result" {
						goto drain
					}
				case <-settle.C:
					slog.Debug("send: settle timeout, no stale result")
					goto drain
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		} else {
			slog.Debug("send: interrupted but idle, skipping settle wait")
		}
	}
drain:
	// Non-blocking drain of any remaining buffered events.
	for {
		select {
		case <-p.eventCh:
		default:
			return nil
		}
	}
}

// Kill forcefully terminates the CLI process via shim.
func (p *Process) Kill() {
	p.killOnce.Do(func() {
		close(p.killCh)
		// Best-effort: send kill command with a short deadline to avoid blocking.
		// If the write fails (conn already broken), the shim's disconnect watchdog
		// will eventually kill the CLI.
		p.shimConn.SetWriteDeadline(time.Now().Add(time.Second)) //nolint:errcheck
		_ = p.shimSend(shimClientMsg{Type: "kill"})
		p.shimConn.Close()
	})
}

// Close gracefully shuts down by closing CLI stdin via shim.
func (p *Process) Close() {
	_ = p.shimSend(shimClientMsg{Type: "close_stdin"})
	timer := time.NewTimer(processCloseTimeout)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		slog.Warn("process close timeout, force killing", "pid", p.cliPID)
		p.Kill()
	}
}

// Detach disconnects from the shim without stopping the CLI.
// Used during naozhi graceful shutdown to keep shim alive.
func (p *Process) Detach() {
	_ = p.shimSend(shimClientMsg{Type: "detach"})
	p.shimConn.Close()
}

// EventEntryFromEvent converts an Event to an EventEntry without appending it.
// Used by both logEvent (real-time) and ReconnectShims (replay injection).
// Returns (entry, ok). ok is false if the event should be skipped.
func EventEntryFromEvent(ev Event) (EventEntry, bool) {
	entry := EventEntry{Time: time.Now().UnixMilli()}

	switch ev.Type {
	case "system":
		entry.Type = "system"
		entry.Summary = ev.SubType
		if ev.SubType == "init" {
			return entry, false
		}
		switch ev.SubType {
		case "task_started":
			entry.Type = "task_start"
			entry.TaskID = ev.TaskID
			entry.ToolUseID = ev.ToolUseID
			if ev.Description != "" {
				entry.Summary = TruncateRunes(ev.Description, 120)
			}
		case "task_progress":
			entry.Type = "task_progress"
			entry.TaskID = ev.TaskID
			entry.ToolUseID = ev.ToolUseID
			if ev.Description != "" {
				entry.Summary = TruncateRunes(ev.Description, 120)
			}
			entry.LastTool = ev.LastToolName
			if ev.Usage != nil {
				entry.ToolUses = ev.Usage.ToolUses
				entry.Tokens = ev.Usage.TotalTokens
				entry.DurationMS = ev.Usage.DurationMS
			}
		case "task_notification":
			entry.Type = "task_done"
			entry.TaskID = ev.TaskID
			entry.ToolUseID = ev.ToolUseID
			if ev.Description != "" {
				entry.Summary = TruncateRunes(ev.Description, 120)
			}
			entry.Status = ev.Status
			if ev.Usage != nil {
				entry.ToolUses = ev.Usage.ToolUses
				entry.Tokens = ev.Usage.TotalTokens
				entry.DurationMS = ev.Usage.DurationMS
			}
		case "stop_hook_summary", "turn_duration", "hook_started", "hook_response":
			return entry, false
		}
	case "assistant":
		if ev.Message == nil {
			return entry, false
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
					entry.Subagent = inp.SubagentType
					if entry.Subagent == "" {
						entry.Subagent = inp.Name
					}
					entry.TeamName = inp.TeamName
					entry.Summary = TruncateRunes(inp.Description, 120)
					entry.Background = inp.RunInBackground
					entry.ToolUseID = block.ID
				}
			case "text":
				entry.Type = "text"
				entry.Summary = TruncateRunes(block.Text, 120)
				entry.Detail = TruncateRunes(block.Text, 16000)
			default:
				continue
			}
			return entry, true
		}
		return entry, false
	case "result":
		entry.Type = "result"
		entry.Summary = TruncateRunes(ev.Result, 200)
		entry.Detail = TruncateRunes(ev.Result, 16000)
		entry.Cost = ev.CostUSD
	default:
		return entry, false
	}

	return entry, true
}

// logEvent converts an Event to an EventEntry and appends it to the event log.
func (p *Process) logEvent(ev Event) {
	entry, ok := EventEntryFromEvent(ev)
	if !ok {
		return
	}
	// Update process-level cost tracking for result events.
	if ev.Type == "result" {
		p.mu.Lock()
		p.totalCost = ev.CostUSD
		p.mu.Unlock()
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

func parseAgentInput(input json.RawMessage) agentInput {
	if len(input) == 0 {
		return agentInput{}
	}
	var inp agentInput
	if err := json.Unmarshal(input, &inp); err != nil {
		slog.Debug("parseAgentInput: unmarshal failed", "err", err)
	}
	return inp
}

func (a agentInput) label() string {
	if a.SubagentType != "" {
		return a.SubagentType
	}
	if a.Name != "" {
		return a.Name
	}
	return a.TeamName
}

func formatToolDetail(block ContentBlock) string {
	if len(block.Input) == 0 {
		return block.Name
	}
	return FormatToolInput(block.Name, block.Input)
}

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

// PID returns the CLI process ID (as reported by shim).
func (p *Process) PID() int {
	return p.cliPID
}

// InjectHistory pre-populates the event log with historical entries.
func (p *Process) InjectHistory(entries []EventEntry) {
	p.eventLog.AppendBatch(entries)
}

// EventEntries returns a copy of all event log entries.
func (p *Process) EventEntries() []EventEntry {
	return p.eventLog.Entries()
}

// EventLastN returns the most recent n event log entries.
func (p *Process) EventLastN(n int) []EventEntry {
	return p.eventLog.LastN(n)
}

// EventEntriesSince returns event log entries after the given unix ms timestamp.
func (p *Process) EventEntriesSince(afterMS int64) []EventEntry {
	return p.eventLog.EntriesSince(afterMS)
}

// TurnAgents returns the sub-agent types spawned in the current turn.
func (p *Process) TurnAgents() []SubagentInfo {
	return p.eventLog.TurnAgents()
}

// SubscribeEvents returns a notification channel and unsubscribe function.
func (p *Process) SubscribeEvents() (<-chan struct{}, func()) {
	return p.eventLog.Subscribe()
}

// LastSeq returns the last received shim sequence number (for reconnect).
func (p *Process) LastSeq() int64 { return p.lastSeq.Load() }
