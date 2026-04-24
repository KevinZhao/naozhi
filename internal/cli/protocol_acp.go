package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ACPProtocol implements Protocol for the Agent Client Protocol (JSON-RPC 2.0).
type ACPProtocol struct {
	mu sync.Mutex
	// nextID is Int64 to avoid sign flip if a very long-running connector
	// ever surpassed 2^31 RPC calls (it currently won't in practice, but the
	// wider type costs nothing and removes the overflow footgun).
	nextID    atomic.Int64
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

func (p *ACPProtocol) Init(rw *JSONRW, resumeID string) (string, error) {
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

	// Step 2: session/new or session/load
	cwd := os.TempDir()
	if resumeID != "" {
		loadID := p.allocID()
		loadReq := RPCRequest{
			JSONRPC: "2.0", ID: loadID, Method: "session/load",
			Params: map[string]any{"sessionId": resumeID, "cwd": cwd},
		}
		if err := p.sendAndWaitResponse(rw, loadReq); err != nil {
			return "", fmt.Errorf("acp session/load: %w", err)
		}
		p.sessionID = resumeID
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
		p.sessionID = result.SessionID
	}

	return p.sessionID, nil
}

func (p *ACPProtocol) WriteMessage(w io.Writer, text string, images []ImageData) error {
	p.mu.Lock()
	p.textBuf.Reset() // reset text accumulator for new turn
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
			"sessionId": p.sessionID,
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

// WriteInterrupt is not supported for ACP. ACP defines session/cancel as an
// RPC method; wiring it in would require turn-scoped request IDs that the
// current wrapper doesn't track. Callers must fall back to Interrupt() (SIGINT).
func (p *ACPProtocol) WriteInterrupt(_ io.Writer, _ string) error {
	return ErrInterruptUnsupported
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

	// Request from agent: session/request_permission
	if msg.IsRequest() && msg.Method == "session/request_permission" {
		ev := Event{Type: "permission_request"}
		if msg.ID != nil {
			ev.RPCRequestID = *msg.ID
		}
		return ev, false, nil
	}

	// Response (turn complete for session/prompt)
	if msg.IsResponse() {
		if msg.Error != nil {
			return Event{}, false, fmt.Errorf("acp rpc error %d: %s", msg.Error.Code, msg.Error.Message)
		}

		p.mu.Lock()
		text := p.textBuf.String()
		p.textBuf.Reset()
		p.mu.Unlock()

		ev := Event{
			Type:      "result",
			Result:    text,
			SessionID: p.sessionID,
		}
		return ev, true, nil
	}

	return Event{}, false, nil
}

type permissionResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      int              `json:"id"`
	Result  permissionResult `json:"result"`
}

type permissionResult struct {
	Outcome permissionOutcome `json:"outcome"`
}

type permissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId"`
}

func (p *ACPProtocol) HandleEvent(w io.Writer, ev Event) bool {
	if ev.Type != "permission_request" {
		return false
	}
	resp := permissionResponse{
		JSONRPC: "2.0",
		ID:      ev.RPCRequestID,
		Result: permissionResult{
			Outcome: permissionOutcome{Outcome: "selected", OptionID: "allow-once"},
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
			if msg.IsResponse() && msg.ID != nil && *msg.ID == expectedID {
				if msg.Error != nil {
					ch <- readResult{nil, fmt.Errorf("rpc error %d: %s", msg.Error.Code, msg.Error.Message)}
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

	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case r := <-ch:
		close(done)
		return r.msg, r.err
	case <-timer.C:
		close(done)
		return nil, fmt.Errorf("timeout waiting for ACP response (id=%d)", expectedID)
	}
}
