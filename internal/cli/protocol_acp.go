package cli

import (
	"encoding/base64"
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

	"github.com/naozhi/naozhi/internal/osutil"
)

// ErrACPRPC wraps any agent-side JSON-RPC error ("error" field populated).
// Typed so dispatch / upstream layers can errors.Is-classify ACP failures
// distinctly from transport / timeout / parse faults.
var ErrACPRPC = errors.New("acp rpc error")

// ErrACPTimeout is returned when waitForResponse gives up on a specific
// JSON-RPC id after the acpHandshakeTimeout deadline. Callers can treat it
// as a transient failure (retry next turn) rather than a permanent protocol
// break.
var ErrACPTimeout = errors.New("acp response timeout")

// acpHandshakeTimeout caps how long ACP RPC waits for a matching response
// before surfacing ErrACPTimeout. Distinct from the unrelated 30s
// shimAuthReadDeadline (shim/server.go) and cronSlowThreshold (cron):
// keeping them named separately avoids cross-tuning by accident.
const acpHandshakeTimeout = 30 * time.Second

// ACPProtocol implements Protocol for the Agent Client Protocol (JSON-RPC 2.0).
type ACPProtocol struct {
	mu sync.Mutex
	// nextID is Int64 to avoid sign flip if a very long-running connector
	// ever surpassed 2^31 RPC calls (it currently won't in practice, but the
	// wider type costs nothing and removes the overflow footgun).
	// NOTE: allocID() narrows to int for RPCRequest.ID/RPCMessage.ID JSON
	// compatibility; 64-bit platforms only (naozhi does not support 32-bit).
	nextID atomic.Int64
	// sessionID is guarded by mu. Init writes once before startReadLoop, but
	// readLoop (ReadEvent) and Send (WriteMessage) goroutines both read it
	// concurrently afterwards, so touches on both reads pair with the single
	// write via mu to satisfy the Go memory model and keep -race quiet.
	sessionID string
	// textBuf accumulates assistant_message_chunk text during a turn
	textBuf strings.Builder
}

func (p *ACPProtocol) Name() string { return "acp" }

func (p *ACPProtocol) Clone() Protocol { return &ACPProtocol{} }

func (p *ACPProtocol) BuildArgs(opts SpawnOptions) []string {
	args := []string{"acp"}
	args = append(args, opts.ExtraArgs...)
	return args
}

func (p *ACPProtocol) Init(rw *JSONRW, resumeID string, cwd string) (string, error) {
	// Step 1: initialize handshake
	initID := p.allocID()
	initReq := RPCRequest{
		JSONRPC: "2.0", ID: initID, Method: "initialize",
		Params: map[string]any{
			"protocolVersion": 1,
			"clientCapabilities": map[string]any{
				"fs":       map[string]bool{"readTextFile": true, "writeTextFile": true},
				"terminal": true,
			},
			"clientInfo": map[string]any{"name": "naozhi", "version": "1.0.0"},
		},
	}
	if err := p.sendAndWaitResponse(rw, initReq); err != nil {
		return "", fmt.Errorf("acp initialize: %w", err)
	}

	// Step 2: session/new or session/load. The cwd passed into Init is the
	// session's workspace (opts.WorkingDir in SpawnOptions); fall back to
	// os.TempDir() only when the caller omitted one (tests, startup probe)
	// so the ACP agent still lands in a valid filesystem location.
	if cwd == "" {
		cwd = os.TempDir()
	}
	if resumeID != "" {
		loadID := p.allocID()
		loadReq := RPCRequest{
			JSONRPC: "2.0", ID: loadID, Method: "session/load",
			Params: map[string]any{"sessionId": resumeID, "cwd": cwd},
		}
		if err := p.sendAndWaitResponse(rw, loadReq); err != nil {
			return "", fmt.Errorf("acp session/load: %w", err)
		}
		p.mu.Lock()
		p.sessionID = resumeID
		p.mu.Unlock()
	} else {
		newID := p.allocID()
		newReq := RPCRequest{
			JSONRPC: "2.0", ID: newID, Method: "session/new",
			Params: map[string]any{"cwd": cwd, "mcpServers": []any{}},
		}
		data, err := json.Marshal(newReq)
		if err != nil {
			return "", err
		}
		if err := rw.WriteLine(data); err != nil {
			return "", err
		}
		// Read responses/notifications until we get the matching response
		resp, err := p.readUntilResponse(rw, newID)
		if err != nil {
			return "", fmt.Errorf("acp session/new: %w", err)
		}
		var result ACPSessionNewResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return "", fmt.Errorf("acp parse session/new result: %w", err)
		}
		p.mu.Lock()
		p.sessionID = result.SessionID
		p.mu.Unlock()
	}

	p.mu.Lock()
	sid := p.sessionID
	p.mu.Unlock()
	return sid, nil
}

func (p *ACPProtocol) WriteMessage(w io.Writer, text string, images []ImageData) error {
	p.mu.Lock()
	p.textBuf.Reset() // reset text accumulator for new turn
	sid := p.sessionID
	p.mu.Unlock()

	// Build prompt content blocks
	var prompt []any
	for _, img := range images {
		prompt = append(prompt, map[string]any{
			"type": "image",
			"source": map[string]string{
				"type":       "base64",
				"media_type": img.MimeType,
				"data":       base64.StdEncoding.EncodeToString(img.Data),
			},
		})
	}
	if text != "" || len(prompt) == 0 {
		prompt = append(prompt, map[string]string{"type": "text", "text": text})
	}

	id := p.allocID()
	req := RPCRequest{
		JSONRPC: "2.0", ID: id, Method: "session/prompt",
		Params: map[string]any{
			"sessionId": sid,
			"prompt":    prompt,
		},
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

// WriteInterrupt sends a session/cancel notification (no id) over stdin to
// abort the in-flight session/prompt. ACP semantics (verified against
// kiro 2.3.0 on 2026-05-18, see docs/rfc/multi-backend-validation.md V1):
//
//   - session/cancel is a JSON-RPC NOTIFICATION (no id field), not a request.
//     Sending it as a request triggered "Method not found" on kiro.
//   - The original session/prompt RPC is then completed with
//     {"result":{"stopReason":"cancelled"}, "id": <prompt-id>} within ms;
//     readLoop already turns that into a normal turn-complete event, so no
//     extra synchronization is required here.
//   - Up to ~10 in-flight chunks may still arrive after the cancel notification
//     (network in-flight); the readLoop tolerates them harmlessly.
//   - The same sessionId immediately accepts the next session/prompt.
//
// Returns ErrInterruptUnsupported only when no session is established yet —
// before the initialize/session_new handshake completes there is no session
// to cancel. Callers see ErrInterruptUnsupported as "fall back to SIGINT"
// (Process.Interrupt).
//
// requestID is ignored: notifications carry no id, so naozhi has no
// correlation to log against. The control_request_id parameter is kept in
// the Protocol interface for the stream-json side (control_request requires
// it).
func (p *ACPProtocol) WriteInterrupt(w io.Writer, _ string) error {
	p.mu.Lock()
	sid := p.sessionID
	p.mu.Unlock()
	if sid == "" {
		return ErrInterruptUnsupported
	}
	notif := struct {
		JSONRPC string         `json:"jsonrpc"`
		Method  string         `json:"method"`
		Params  map[string]any `json:"params"`
	}{
		JSONRPC: "2.0",
		Method:  "session/cancel",
		Params:  map[string]any{"sessionId": sid},
	}
	data, err := json.Marshal(notif)
	if err != nil {
		return fmt.Errorf("acp marshal session/cancel: %w", err)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("acp write session/cancel: %w", err)
	}
	return nil
}

// WriteUserMessageLocked ignores uuid and priority — ACP has neither concept.
// Sessions whose protocol has SupportsReplay()==false fall back to Collect
// mode regardless of queue.mode config (see dispatcher selection logic).
func (p *ACPProtocol) WriteUserMessageLocked(w io.Writer, _, text string, images []ImageData, _ string) error {
	return p.WriteMessage(w, text, images)
}

func (p *ACPProtocol) SupportsPriority() bool { return false }
func (p *ACPProtocol) SupportsReplay() bool   { return false }

// Capabilities returns the hard-coded Caps for ACP JSON-RPC.
// ACP has no stdin-level interrupt but session/cancel is a safe soft
// cancel RPC, so SoftInterrupt=true even though WriteInterrupt
// currently returns ErrInterruptUnsupported. See RNEW-ARCH-404.
func (p *ACPProtocol) Capabilities() Caps {
	return Caps{Replay: false, Priority: false, SoftInterrupt: true, StreamJSON: false}
}

func (p *ACPProtocol) ReadEvent(line string) (Event, bool, error) {
	var msg RPCMessage
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return Event{}, false, err
	}

	// Notification: session/update
	if msg.IsNotification() && msg.Method == "session/update" {
		return p.parseSessionUpdate(msg.Params)
	}

	// Request from agent: session/request_permission.
	// IDAsString tolerates kiro's UUID strings as well as numeric ids; HandleEvent
	// echoes whatever the agent sent back verbatim. RawParams carries the raw
	// options[] so HandleEvent can pick the optionId by kind without hardcoding
	// the vendor-specific identifier.
	if msg.IsRequest() && msg.Method == "session/request_permission" {
		ev := Event{Type: "permission_request", RawParams: msg.Params}
		if id, ok := msg.IDAsString(); ok {
			ev.RPCRequestID = id
		}
		return ev, false, nil
	}

	// Response (turn complete for session/prompt)
	if msg.IsResponse() {
		if msg.Error != nil {
			// R184-SEC-M1: msg.Error.Message comes from the ACP agent (kiro /
			// Gemini CLI / etc), a separate trust boundary. The error string
			// flows into slog attrs (`readLoop` Warn) and surfaces on the
			// dashboard, so untrusted control characters / bidi overrides must
			// be scrubbed before they reach structured logs. Matches the
			// R172-SEC-M4 / R175-SEC-P1 / R183-SEC-H1 sanitize policy.
			return Event{}, false, fmt.Errorf("%w %d: %s", ErrACPRPC,
				msg.Error.Code, osutil.SanitizeForLog(msg.Error.Message, 256))
		}

		// Decode the optional stopReason so callers can distinguish a normal
		// turn end from a cancelled one. ACP spec values: "end_turn",
		// "cancelled", "max_tokens", "tool_use_failure", "refusal". We expose
		// the raw string in SubType — same field used by stream-json events.
		var stop struct {
			StopReason string `json:"stopReason"`
		}
		_ = json.Unmarshal(msg.Result, &stop) // best-effort; missing => empty

		p.mu.Lock()
		text := p.textBuf.String()
		p.textBuf.Reset()
		sid := p.sessionID
		p.mu.Unlock()

		ev := Event{
			Type:      "result",
			SubType:   stop.StopReason,
			Result:    text,
			SessionID: sid,
		}
		return ev, true, nil
	}

	return Event{}, false, nil
}

// permissionResponse encodes a JSON-RPC response to session/request_permission.
// ID is json.RawMessage so the original agent-supplied id (UUID string for
// kiro 2.3.0, int for some implementations) is round-tripped verbatim — the
// JSON-RPC spec requires the response id to match the request id exactly,
// including type. See V7b in docs/rfc/multi-backend-validation.md.
type permissionResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      json.RawMessage  `json:"id"`
	Result  permissionResult `json:"result"`
}

type permissionResult struct {
	Outcome permissionOutcome `json:"outcome"`
}

type permissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId"`
}

// pickAllowOptionID returns the optionId of the first allow_* kind in opts,
// or empty string when none match. Reading the optionId from the request is
// required because vendor implementations differ on the exact identifier:
// kiro 2.3.0 emits underscored names (allow_once / allow_always / reject_once)
// while older ACP drafts documented hyphenated forms. Hardcoding either form
// breaks the other backend silently.
func pickAllowOptionID(opts []ACPPermissionOption) string {
	// Prefer allow_once over allow_always so naozhi never auto-grants
	// persistent permissions on behalf of the user.
	for _, o := range opts {
		if o.Kind == "allow_once" {
			return o.OptionID
		}
	}
	for _, o := range opts {
		if o.Kind == "allow_always" {
			return o.OptionID
		}
	}
	// Last resort: any option whose kind starts with "allow"
	for _, o := range opts {
		if strings.HasPrefix(o.Kind, "allow") {
			return o.OptionID
		}
	}
	return ""
}

func (p *ACPProtocol) HandleEvent(w io.Writer, ev Event) bool {
	if ev.Type != "permission_request" {
		return false
	}
	// Pick optionId from the request's options[] rather than hardcoding.
	// Falls back to "allow_once" string when params parsing fails — better to
	// guess than to leave a permission_request unanswered (which stalls the
	// turn indefinitely). The unknown-vendor branch is exercised in tests.
	chosen := "allow_once"
	if len(ev.RawParams) > 0 {
		var params ACPPermissionRequestParams
		if err := json.Unmarshal(ev.RawParams, &params); err == nil {
			if id := pickAllowOptionID(params.Options); id != "" {
				chosen = id
			} else {
				slog.Warn("acp: permission_request has no allow_* option, falling back",
					"options", len(params.Options),
					"chosen", chosen)
			}
		} else {
			slog.Warn("acp: failed to parse permission_request params", "err", err)
		}
	}

	// id may be empty when the original request had no id (a malformed
	// request from the agent). Echo back json null so the JSON-RPC spec is
	// at least syntactically honored.
	idRaw := json.RawMessage(`null`)
	if ev.RPCRequestID != "" {
		// Try int first to mirror the wire shape; fall back to string.
		if n, err := strconv.Atoi(ev.RPCRequestID); err == nil {
			idRaw = json.RawMessage(strconv.Itoa(n))
		} else {
			b, _ := json.Marshal(ev.RPCRequestID)
			idRaw = b
		}
	}

	resp := permissionResponse{
		JSONRPC: "2.0",
		ID:      idRaw,
		Result: permissionResult{
			Outcome: permissionOutcome{Outcome: "selected", OptionID: chosen},
		},
	}
	data, err := json.Marshal(resp)
	if err != nil {
		slog.Warn("acp: failed to marshal permission response", "err", err)
		return true
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		slog.Warn("acp: failed to send permission response", "err", err)
	}
	return true
}

func (p *ACPProtocol) parseSessionUpdate(params json.RawMessage) (Event, bool, error) {
	var update ACPSessionUpdate
	if err := json.Unmarshal(params, &update); err != nil {
		return Event{}, false, err
	}

	switch update.Update.SessionUpdate {
	case "agent_message_chunk":
		var content ACPTextContent
		if err := json.Unmarshal(update.Update.Content, &content); err != nil {
			// Log the raw payload so diagnosing an upstream schema drift does
			// not require reproducing the session; without this the user sees
			// an empty reply and we have no trail.
			slog.Warn("acp: agent_message_chunk content unmarshal failed",
				"err", err,
				"raw_len", len(update.Update.Content))
		} else if content.Text != "" {
			p.mu.Lock()
			p.textBuf.WriteString(content.Text)
			p.mu.Unlock()
		}
		return Event{Type: "assistant", SessionID: update.SessionID}, false, nil

	case "tool_call":
		return Event{
			Type:    "assistant",
			SubType: "tool_use",
			Message: &AssistantMessage{
				Content: []ContentBlock{{Type: "tool_use", Name: update.Update.Title}},
			},
		}, false, nil

	case "tool_call_update":
		return Event{Type: "assistant", SubType: "tool_result"}, false, nil

	default:
		return Event{Type: "system", SubType: update.Update.SessionUpdate}, false, nil
	}
}

// allocID returns a monotonically increasing RPC id.
//
// R185-GO-L1: the narrowing from int64 → int is a deliberate contract —
// RPCRequest.ID and RPCMessage.ID are `int` to keep JSON marshaling
// idiomatic, and on 64-bit platforms (the only naozhi build target) int
// is a full 64-bit word so the conversion is lossless for any id the
// connector can produce in its lifetime. On a 32-bit target the top 32
// bits would silently truncate and collide with earlier ids; we document
// this here rather than adding a runtime guard because cross-compiling
// naozhi to 32-bit is not supported.
func (p *ACPProtocol) allocID() int {
	return int(p.nextID.Add(1) - 1)
}

func (p *ACPProtocol) sendAndWaitResponse(rw *JSONRW, req RPCRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if err := rw.WriteLine(data); err != nil {
		return err
	}
	_, err = p.readUntilResponse(rw, req.ID)
	return err
}

// readUntilResponse reads lines until a JSON-RPC response with the matching ID is found.
// Notifications are silently consumed during this process.
// Times out after 30 seconds to prevent deadlocking the caller.
func (p *ACPProtocol) readUntilResponse(rw *JSONRW, expectedID int) (*RPCMessage, error) {
	type readResult struct {
		msg *RPCMessage
		err error
	}
	ch := make(chan readResult, 1)
	done := make(chan struct{})
	go func() {
		for {
			line, eof, err := rw.R.ReadLine()
			if err != nil || eof {
				ch <- readResult{nil, fmt.Errorf("unexpected EOF during ACP init")}
				return
			}
			if len(line) == 0 {
				continue
			}
			var msg RPCMessage
			if err := json.Unmarshal(line, &msg); err != nil {
				continue
			}
			// expectedID is int because naozhi-originated requests always use
			// int ids (see allocID). msg.ID is RawMessage to tolerate string
			// ids on agent-originated requests; for matching responses we
			// expect numeric round-trip.
			gotID, gotOK := msg.IDAsInt()
			if msg.IsResponse() && gotOK && gotID == expectedID {
				if msg.Error != nil {
					// R184-SEC-M1: sanitize RPC error text before it bubbles
					// up through caller slog attrs. See ReadEvent above.
					ch <- readResult{nil, fmt.Errorf("%w %d: %s", ErrACPRPC,
						msg.Error.Code, osutil.SanitizeForLog(msg.Error.Message, 256))}
					return
				}
				ch <- readResult{&msg, nil}
				return
			}
			// Check if caller gave up (timeout). The goroutine will be fully
			// freed when the process pipe closes; this just avoids useless work.
			select {
			case <-done:
				return
			default:
			}
		}
	}()

	timer := time.NewTimer(acpHandshakeTimeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		close(done)
		return r.msg, r.err
	case <-timer.C:
		close(done)
		// R184-CONCUR-H1: `done` is only polled between ReadLine calls; a
		// reader parked inside the underlying bufio.ReadBytes syscall never
		// observes it. If the goroutine has a shim-backed reader, poke the
		// underlying net.Conn's read deadline so ReadBytes returns
		// immediately with i/o timeout, letting the reader goroutine exit
		// instead of lingering for the lifetime of the shim connection.
		if sl, ok := rw.R.(*shimLineReader); ok && sl.proc != nil && sl.proc.shimConn != nil {
			// Pulse the deadline to unblock any in-flight ReadBytes, then
			// clear it so subsequent operations on shimConn (e.g. if caller
			// fails to Kill/Close promptly) are not prematurely cancelled.
			// The reader goroutine observing EOF/err is what we want — not
			// permanently arming an expired deadline.
			_ = sl.proc.shimConn.SetReadDeadline(time.Now())
			_ = sl.proc.shimConn.SetReadDeadline(time.Time{})
		}
		return nil, fmt.Errorf("%w (id=%d)", ErrACPTimeout, expectedID)
	}
}
