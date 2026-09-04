package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
)

// codexHandshakeTimeout caps handshake RPC waits (initialize / thread/start /
// thread/resume). Separate from acpHandshakeTimeout; codex model warmup is slower.
const codexHandshakeTimeout = 45 * time.Second

// CodexProtocol implements Protocol for OpenAI's `codex app-server` (JSON-RPC
// 2.0 over stdio NDJSON). It shares ACPProtocol's "long RPC + interleaved
// notifications" shape but uses codex's own methods and payloads, so it is a
// SEPARATE implementation (docs/rfc/codex-backend.md §1.3). Wire contract
// validated against codex-cli 0.141.0 (docs/rfc/codex-backend-validation.md).
type CodexProtocol struct {
	mu sync.Mutex
	// nextID mirrors ACPProtocol.nextID (int64, narrowed to int in allocID; 64-bit only).
	nextID atomic.Int64
	// threadID is written once by Init (thread/start result.thread.id) and read
	// concurrently; atomic.Pointer so reads never contend with textBuf writes under mu.
	threadID atomic.Pointer[string]
	// textBuf accumulates item/agentMessage/delta text, flushed at turn/completed. Guarded by mu.
	textBuf strings.Builder
	// BackendID labels per-backend metric increments (RFC multi-backend §10).
	BackendID string
}

// ErrCodexRPC wraps a JSON-RPC error returned by codex app-server.
var ErrCodexRPC = errors.New("codex rpc error")

// ErrCodexTimeout is returned when readUntilResponse times out on an id; treated as transient.
var ErrCodexTimeout = errors.New("codex response timeout")

func (p *CodexProtocol) Name() string { return "codex" }

// Clone returns a fresh instance retaining BackendID so metric labelling survives proto.Clone().
func (p *CodexProtocol) Clone() Protocol { return &CodexProtocol{BackendID: p.BackendID} }

func (p *CodexProtocol) storeThreadID(id string) { p.threadID.Store(&id) }

func (p *CodexProtocol) loadThreadID() string {
	if s := p.threadID.Load(); s != nil {
		return *s
	}
	return ""
}

func (p *CodexProtocol) allocID() int { return int(p.nextID.Add(1) - 1) }

// BuildArgs launches `codex app-server`; model / sandbox / approval flow via
// config (-c). approval_policy=never + sandbox_mode=workspace-write mirrors
// claude's --dangerously-skip-permissions; danger-full-access is NOT used
// because codex 0.141 then silently falls back to read-only (validation §2.5).
func (p *CodexProtocol) BuildArgs(opts SpawnOptions) []string {
	args := []string{"app-server"}
	if opts.Model != "" {
		args = append(args, "-c", "model="+opts.Model)
	}
	args = append(args, "-c", "approval_policy=never")
	args = append(args, "-c", "sandbox_mode=workspace-write")
	// Mirror ClaudeProtocol's ARG_MAX defence so codex extra args can't bypass it.
	args = append(args, capExtraArgsBytes(opts.ExtraArgs)...)
	return args
}

// --- typed RPC param/result shapes (validated 2026-06-21) ---

type codexInitParams struct {
	ClientInfo codexClientInfo `json:"clientInfo"`
}

type codexClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type codexThreadStartParams struct {
	Cwd string `json:"cwd"`
}

type codexThreadResumeParams struct {
	ThreadID string `json:"threadId"`
	Cwd      string `json:"cwd"`
}

// codexThreadStartResult decodes thread/start's response. threadId lives at
// result.thread.id (validation §3.1) — NOT in the thread/started notification.
type codexThreadStartResult struct {
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
}

// codexUserInput is one entry of turn/start's UserInput[] (validation §3.2);
// a bare string yields -32600 "expected a sequence".
type codexUserInput struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type codexTurnStartParams struct {
	ThreadID string           `json:"threadId"`
	Input    []codexUserInput `json:"input"`
}

type codexTurnInterruptParams struct {
	ThreadID string `json:"threadId"`
}

func (p *CodexProtocol) Init(rw *JSONRW, resumeID string, cwd string) (string, error) {
	initReq := RPCRequest{
		JSONRPC: "2.0", ID: p.allocID(), Method: "initialize",
		Params: codexInitParams{ClientInfo: codexClientInfo{Name: "naozhi", Version: "1.0.0"}},
	}
	if _, err := p.sendAndWaitResponse(rw, initReq); err != nil {
		return "", fmt.Errorf("codex initialize: %w", err)
	}

	// Until `initialized` (no id) is sent the server rejects every method with "Not initialized".
	if err := p.writeNotification(rw.W, "initialized", nil); err != nil {
		return "", fmt.Errorf("codex initialized notify: %w", err)
	}

	if cwd == "" {
		cwd = os.TempDir()
	}
	if resumeID != "" {
		resumeReq := RPCRequest{
			JSONRPC: "2.0", ID: p.allocID(), Method: "thread/resume",
			Params: codexThreadResumeParams{ThreadID: resumeID, Cwd: cwd},
		}
		resp, err := p.sendAndWaitResponse(rw, resumeReq)
		if err != nil {
			return "", fmt.Errorf("codex thread/resume: %w", err)
		}
		tid := resumeID
		if resp != nil && len(resp.Result) > 0 {
			var r codexThreadStartResult
			if json.Unmarshal(resp.Result, &r) == nil && r.Thread.ID != "" {
				tid = r.Thread.ID
			}
		}
		p.storeThreadID(tid)
		return tid, nil
	}

	startReq := RPCRequest{
		JSONRPC: "2.0", ID: p.allocID(), Method: "thread/start",
		Params: codexThreadStartParams{Cwd: cwd},
	}
	resp, err := p.sendAndWaitResponse(rw, startReq)
	if err != nil {
		return "", fmt.Errorf("codex thread/start: %w", err)
	}
	var result codexThreadStartResult
	if resp == nil || json.Unmarshal(resp.Result, &result) != nil || result.Thread.ID == "" {
		return "", fmt.Errorf("codex thread/start: missing thread id in result")
	}
	p.storeThreadID(result.Thread.ID)
	return result.Thread.ID, nil
}

func (p *CodexProtocol) WriteMessage(w io.Writer, text string, images []Attachment) error {
	tid := p.loadThreadID()
	p.mu.Lock()
	p.textBuf.Reset()
	p.mu.Unlock()

	input := make([]codexUserInput, 0, len(images)+1)
	for _, img := range images {
		// Image input is a data: URL; only the gpt-5.x path accepts images, the
		// gpt-oss Bedrock path does not (validation §4).
		input = append(input, codexUserInput{
			Type:     "image",
			ImageURL: "data:" + img.MimeType + ";base64," + encodeImageBase64(img.Data),
		})
	}
	if text != "" || len(input) == 0 {
		input = append(input, codexUserInput{Type: "text", Text: text})
	}

	req := RPCRequest{
		JSONRPC: "2.0", ID: p.allocID(), Method: "turn/start",
		Params: codexTurnStartParams{ThreadID: tid, Input: input},
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// WriteInterrupt sends a turn/interrupt request (unlike ACP's notification) to
// abort the in-flight turn. Fire-and-forget under the caller-held write lock;
// readLoop observes the resulting turn/completed. Returns
// ErrInterruptUnsupported before a thread exists (callers fall back to SIGINT).
func (p *CodexProtocol) WriteInterrupt(w io.Writer, _ string) error {
	tid := p.loadThreadID()
	if tid == "" {
		return ErrInterruptUnsupported
	}
	req := RPCRequest{
		JSONRPC: "2.0", ID: p.allocID(), Method: "turn/interrupt",
		Params: codexTurnInterruptParams{ThreadID: tid},
	}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("codex marshal turn/interrupt: %w", err)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("codex write turn/interrupt: %w", err)
	}
	// Only successful cancels count; the pre-handshake return never reached the agent.
	metrics.RecordACPCancel(p.BackendID)
	return nil
}

// WriteUserMessageLocked ignores uuid/priority — codex has no replay/priority concept (RFC §3).
func (p *CodexProtocol) WriteUserMessageLocked(w io.Writer, _, text string, images []Attachment, _ string) error {
	return p.WriteMessage(w, text, images)
}

func (p *CodexProtocol) SupportsPriority() bool { return false }
func (p *CodexProtocol) SupportsReplay() bool   { return false }

// Capabilities: SoftInterrupt via turn/interrupt once a thread exists; not
// stream-json. EffortTier=false: codex's reasoning control rides on
// `-c model_reasoning_effort=`, a different axis this RFC does not model.
func (p *CodexProtocol) Capabilities() Caps {
	return Caps{Replay: false, Priority: false, SoftInterrupt: true, StreamJSON: false,
		EffortTier: false}
}

// --- notification decode shapes ---

type codexAgentMessageDelta struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

type codexItemNotif struct {
	ThreadID string          `json:"threadId"`
	Item     codexThreadItem `json:"item"`
}

type codexThreadItem struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Text   string `json:"text,omitempty"`
	Status string `json:"status,omitempty"`
	Title  string `json:"title,omitempty"`
}

type codexTurnCompleted struct {
	ThreadID string `json:"threadId"`
	Turn     struct {
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	} `json:"turn"`
}

type codexTokenUsageNotif struct {
	ThreadID   string `json:"threadId"`
	TokenUsage struct {
		Last struct {
			InputTokens  int64 `json:"inputTokens"`
			OutputTokens int64 `json:"outputTokens"`
			TotalTokens  int64 `json:"totalTokens"`
		} `json:"last"`
		ModelContextWindow *int64 `json:"modelContextWindow"`
	} `json:"tokenUsage"`
}

func (p *CodexProtocol) ReadEvent(line string) ([]Event, bool, error) {
	var msg RPCMessage
	if err := json.Unmarshal(stringToBytesUnsafe(line), &msg); err != nil {
		return nil, false, err
	}

	if msg.IsNotification() {
		return p.handleNotification(msg)
	}

	// Reverse requests (id AND method): approvals become a synthetic
	// permission_request so HandleEvent can auto-allow; the id round-trips.
	if msg.IsRequest() {
		if strings.HasSuffix(msg.Method, "/requestApproval") {
			rid, _ := msg.IDAsString()
			return []Event{{
				Type:         "permission_request",
				SubType:      msg.Method,
				RPCRequestID: rid,
				RawParams:    msg.Params,
			}}, false, nil
		}
		// requestUserInput / elicitation are not wired to cards yet; ignore so the
		// turn does not hang on our side — codex applies its own default on timeout.
		slog.Debug("codex: unhandled reverse request", "method", msg.Method)
		return nil, false, nil
	}

	// The only id-bearing response on the readLoop is the deferred turn/start
	// reply. Success carries nothing visible, but an ERROR must close the turn
	// and be recorded or the session hangs in state=running (mirrors ACP).
	if msg.IsResponse() && msg.Error != nil {
		metrics.RecordProtocolRPCError(p.BackendID, "turn/start", strconv.Itoa(msg.Error.Code))
		// Discard partial text and surface a synthetic error so readLoop closes the turn.
		p.mu.Lock()
		p.textBuf.Reset()
		p.mu.Unlock()
		return nil, true, fmt.Errorf("%w %d: %s", ErrCodexRPC,
			msg.Error.Code, osutil.SanitizeForLog(msg.Error.Message, 256))
	}
	return nil, false, nil
}

func (p *CodexProtocol) handleNotification(msg RPCMessage) ([]Event, bool, error) {
	switch msg.Method {
	case "item/agentMessage/delta":
		var d codexAgentMessageDelta
		if err := json.Unmarshal(msg.Params, &d); err != nil {
			slog.Warn("codex: agentMessage delta unmarshal failed", "err", err)
			return nil, false, nil
		}
		if d.Delta != "" {
			p.mu.Lock()
			if room := maxAssistantMessageContentBytes - p.textBuf.Len(); room > 0 {
				if len(d.Delta) <= room {
					p.textBuf.WriteString(d.Delta)
				} else {
					n := textutil.TruncateAtRuneBoundary(d.Delta, room)
					p.textBuf.WriteString(d.Delta[:n])
				}
			}
			p.mu.Unlock()
		}
		return []Event{{Type: "assistant", SessionID: d.ThreadID}}, false, nil

	case "item/started", "item/completed":
		var n codexItemNotif
		if err := json.Unmarshal(msg.Params, &n); err != nil {
			return nil, false, nil
		}
		switch n.Item.Type {
		case "commandExecution", "fileChange", "mcpToolCall", "webSearch", "dynamicToolCall":
			subType := "tool_use"
			if msg.Method == "item/completed" {
				subType = "tool_result"
			}
			return []Event{{
				Type:      "assistant",
				SubType:   subType,
				SessionID: n.ThreadID,
				ToolUseID: n.Item.ID,
				ToolCall: &clievent.ToolCall{
					ID:     n.Item.ID,
					Title:  sanitizeToolCallLabel(n.Item.Title),
					Kind:   sanitizeToolCallLabel(n.Item.Type),
					Status: sanitizeToolCallLabel(n.Item.Status),
				},
				Message: &AssistantMessage{
					Content: []ContentBlock{{Type: "tool_use", Name: sanitizeToolCallLabel(n.Item.Type)}},
				},
			}}, false, nil
		default:
			// userMessage / agentMessage / reasoning / plan lifecycle markers carry
			// no visible payload (text streams via deltas); skip.
			return nil, false, nil
		}

	case "thread/tokenUsage/updated":
		var u codexTokenUsageNotif
		if err := json.Unmarshal(msg.Params, &u); err != nil {
			return nil, false, nil
		}
		meta := &EventMetadata{}
		if u.TokenUsage.Last.TotalTokens > 0 {
			meta.MeteringUsage = []MeteringEntry{{
				Value:      float64(u.TokenUsage.Last.TotalTokens),
				Unit:       "token",
				UnitPlural: "tokens",
			}}
		}
		if u.TokenUsage.ModelContextWindow != nil && *u.TokenUsage.ModelContextWindow > 0 {
			meta.ContextUsagePercent = normalizeContextUsage(
				float64(u.TokenUsage.Last.TotalTokens) / float64(*u.TokenUsage.ModelContextWindow))
		}
		return []Event{{Type: "metadata", SessionID: u.ThreadID, Metadata: meta}}, false, nil

	case "turn/completed":
		var c codexTurnCompleted
		_ = json.Unmarshal(msg.Params, &c) // best-effort; even on decode failure the turn is over
		p.mu.Lock()
		text := p.textBuf.String()
		p.textBuf.Reset()
		p.mu.Unlock()

		// Turn boundary emits up to TWO events (mirrors ACP): an assistant frame
		// with the accumulated text — the ONLY place the visible reply materialises
		// — and a result event whose Result feeds SendResult.Text only.
		var events []Event
		if text != "" {
			events = append(events, Event{
				Type:      "assistant",
				SessionID: c.ThreadID,
				Message: &AssistantMessage{
					Content: []ContentBlock{{Type: "text", Text: text}},
				},
			})
		}
		result := Event{Type: "result", SessionID: c.ThreadID, Result: text}
		if c.Turn.Status == "failed" && c.Turn.Error != nil {
			// Failure reason goes in the result; the assistant frame keeps partial text.
			result.SubType = "error"
			result.Result = osutil.SanitizeForLog(c.Turn.Error.Message, 1024)
		}
		events = append(events, result)
		return events, true, nil

	case "error":
		// Stream-level error (e.g. provider 5xx); turn/completed status:failed follows.
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(msg.Params, &e)
		slog.Warn("codex: error notification", "msg", osutil.SanitizeForLog(e.Error.Message, 256))
		return nil, false, nil

	case "thread/started", "turn/started", "thread/status/changed",
		"configWarning", "warning", "remoteControl/status/changed",
		"item/reasoning/textDelta", "item/reasoning/summaryTextDelta",
		"turn/plan/updated", "turn/diff/updated":
		// Known-but-uninteresting lifecycle / reasoning / plan frames.
		return nil, false, nil

	default:
		slog.Debug("codex: unknown notification", "method", msg.Method)
		return nil, false, nil
	}
}

func (p *CodexProtocol) HandleEvent(w io.Writer, ev Event) bool {
	if ev.Type != "permission_request" {
		return false
	}
	// Auto-allow every */requestApproval with {decision: "approved"}. Ids may be
	// numeric or string: an integer-parsable source string is already a valid
	// JSON number literal, so reuse it verbatim; otherwise quote via json.Marshal.
	idRaw := json.RawMessage(`null`)
	if ev.RPCRequestID != "" {
		if _, err := strconv.Atoi(ev.RPCRequestID); err == nil {
			idRaw = json.RawMessage(ev.RPCRequestID)
		} else if b, err := json.Marshal(ev.RPCRequestID); err == nil {
			idRaw = b
		}
	}
	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			Decision string `json:"decision"`
		} `json:"result"`
	}{JSONRPC: "2.0", ID: idRaw}
	resp.Result.Decision = "approved"
	data, err := json.Marshal(resp)
	if err != nil {
		slog.Warn("codex: marshal approval response failed", "err", err)
		return true
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		slog.Warn("codex: write approval response failed", "err", err)
	}
	return true
}

// writeNotification emits a JSON-RPC notification (no id) on stdin.
func (p *CodexProtocol) writeNotification(w io.Writer, method string, params any) error {
	msg := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{JSONRPC: "2.0", Method: method, Params: params}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// sendAndWaitResponse writes req then blocks until the matching response
// arrives, consuming interleaved notifications. turn/start is NOT sent through
// here: its response is deferred past all item/* notifications (see ReadEvent).
func (p *CodexProtocol) sendAndWaitResponse(rw *JSONRW, req RPCRequest) (*RPCMessage, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if err := rw.WriteLine(data); err != nil {
		return nil, err
	}
	resp, err := p.readUntilResponse(rw, req.ID)
	if err != nil {
		metrics.RecordProtocolRPCError(p.BackendID, req.Method, "")
	}
	return resp, err
}

// readUntilResponse reads lines until the response with the matching id
// arrives, consuming notifications. Modelled on ACPProtocol.readUntilResponse
// but kept separate so a codex schema change cannot perturb the kiro path.
func (p *CodexProtocol) readUntilResponse(rw *JSONRW, expectedID int) (*RPCMessage, error) {
	type readResult struct {
		msg *RPCMessage
		err error
	}
	ch := make(chan readResult, 1)
	var done atomic.Bool
	send := func(r readResult) {
		if done.Load() {
			return
		}
		select {
		case ch <- r:
		default:
		}
	}
	go func() {
		for {
			line, eof, err := rw.R.ReadLine()
			if err != nil {
				send(readResult{nil, fmt.Errorf("read codex response: %w", err)})
				return
			}
			if eof {
				send(readResult{nil, fmt.Errorf("unexpected EOF during codex init")})
				return
			}
			if len(line) == 0 {
				continue
			}
			var msg RPCMessage
			if err := json.Unmarshal(line, &msg); err != nil {
				continue
			}
			gotID, gotOK := msg.IDAsInt()
			if msg.IsResponse() && gotOK && gotID == expectedID {
				if msg.Error != nil {
					send(readResult{nil, fmt.Errorf("%w %d: %s", ErrCodexRPC,
						msg.Error.Code, osutil.SanitizeForLog(msg.Error.Message, 256))})
					return
				}
				send(readResult{&msg, nil})
				return
			}
			if done.Load() {
				return
			}
		}
	}()

	timer := time.NewTimer(codexHandshakeTimeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		done.Store(true)
		return r.msg, r.err
	case <-timer.C:
		done.Store(true)
		// Mirror ACP's read-deadline pulse so a reader parked in bufio.ReadBytes unblocks.
		if sl, ok := rw.R.(*shimLineReader); ok && sl.proc != nil && sl.proc.shimConn != nil {
			_ = sl.proc.shimConn.SetReadDeadline(time.Now())
			_ = sl.proc.shimConn.SetReadDeadline(time.Time{})
		}
		// Non-shim readers leak the goroutine until the pipe closes (same limitation as ACP).
		return nil, fmt.Errorf("%w (id=%d)", ErrCodexTimeout, expectedID)
	}
}
