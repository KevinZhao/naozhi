// protocol_acp.go is the production ACP (Agent Client Protocol) backend
// adapter: the kiro backend uses it via internal/cli/backend/profile_kiro.go,
// so it must not be build-tagged out without also retiring the kiro profile
// (#629). Future ACP-flavoured backends (Gemini, custom JSON-RPC peers)
// extend this adapter.
package cli

import (
	"bytes"
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

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
)

// acpStopReasonKey is the on-wire key ACP uses in a session/prompt response
// Result for the turn-end reason. Most kiro responses are `null` or `{}`, so
// a byte check before json.Unmarshal skips reflect+alloc on the hot path.
var acpStopReasonKey = []byte(`"stopReason"`)

// acpEncBuf bundles a *bytes.Buffer + *json.Encoder into one pooled pair so the
// WriteMessage / permissionResponse hot paths skip the per-call json.Marshal
// allocator. Mirrors process_shim_io.go's shimSendBufPool.
type acpEncBuf struct {
	buf *bytes.Buffer
	enc *json.Encoder
}

// acpEncPool reuses encoder/buffer pairs across ACP send calls; the encoder
// writes via the buffer pointer so a single Reset between uses is safe.
var acpEncPool = sync.Pool{
	New: func() any {
		buf := new(bytes.Buffer)
		enc := json.NewEncoder(buf)
		// User prompts / permission selections may legitimately contain '<', '>',
		// '&'; default HTML escaping would corrupt them (mirrors shimSendBufPool).
		enc.SetEscapeHTML(false)
		return &acpEncBuf{buf: buf, enc: enc}
	},
}

// acpEncBufMaxCap caps the pool entry capacity; image-bearing prompts inflate
// a buffer to >256KB and oversized entries are dropped on Put.
const acpEncBufMaxCap = 64 * 1024

func putACPEncBuf(e *acpEncBuf) {
	if e.buf.Cap() > acpEncBufMaxCap {
		return
	}
	acpEncPool.Put(e)
}

// acpB64BufPool reuses []byte scratch buffers for base64-encoding per-image
// payloads via AppendEncode. The final string conversion still allocates (the
// Data field is `string`) but the encode buffer is amortised. A 16KB entry
// covers the 1-image case; outliers grow and are discarded (acpB64BufMaxCap).
var acpB64BufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 16*1024)
		return &b
	},
}

// acpB64BufMaxCap matches acpEncBufMaxCap; buffers grown past it are dropped on Put.
const acpB64BufMaxCap = 64 * 1024

// encodeImageBase64 base64-encodes img via a pooled scratch buffer; the
// returned string is fresh (the marshaller's `Data string` field needs it).
func encodeImageBase64(img []byte) string {
	bp := acpB64BufPool.Get().(*[]byte)
	buf := (*bp)[:0]
	buf = base64.StdEncoding.AppendEncode(buf, img)
	out := string(buf)
	if cap(buf) <= acpB64BufMaxCap {
		*bp = buf[:0]
		acpB64BufPool.Put(bp)
	}
	return out
}

// toolJSONMaxRunes caps tool_call input/output payloads in Event.ToolCall
// before dashboard / IM rendering. 16 KiB holds a typical Read / Bash / Edit
// invocation in full while keeping a runaway tool from blowing up WS frames
// and slog attrs; aligned with process_event_format.go's full-content cap.
const toolJSONMaxRunes = 16000

// truncateToolJSON converts raw JSON to a string capped at toolJSONMaxRunes
// (with "..." when truncated). TruncateRunesBytes elides the heap copy in the
// common truncating case; nil / empty input returns "" (field is optional).
func truncateToolJSON(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return textutil.TruncateRunesBytes(b, toolJSONMaxRunes)
}

// toolCallLabelMaxBytes caps ToolCall.Title / Kind / Status before they reach
// dashboard / IM renderers. They come from the agent (untrusted across the
// protocol boundary) and render into chip text/colour unescaped, so a hostile
// agent could distort the UI with bidi-override or oversized strings.
const toolCallLabelMaxBytes = 256

// sanitizeToolCallLabel cleans a short ACP-supplied label field. LOSSY: control
// and bidi/LS-PS runes become `_` (osutil.SanitizeForLog) and the result is
// byte-capped at toolCallLabelMaxBytes; callers needing the verbatim string
// must use another field (e.g. truncateToolJSON). Empty stays empty.
func sanitizeToolCallLabel(s string) string {
	if s == "" {
		return ""
	}
	return osutil.SanitizeForLog(s, toolCallLabelMaxBytes)
}

// ErrACPRPC wraps any agent-side JSON-RPC error, typed so dispatch / upstream
// layers can errors.Is-classify it apart from transport / timeout / parse faults.
var ErrACPRPC = errors.New("acp rpc error")

// ErrACPTimeout is returned when readUntilResponse gives up on a JSON-RPC id
// after acpHandshakeTimeout. Callers may treat it as transient (retry next turn).
var ErrACPTimeout = errors.New("acp response timeout")

// acpHandshakeTimeout caps how long an ACP RPC waits for its response. Named
// separately from shimAuthReadDeadline / cronSlowThreshold to avoid cross-tuning.
const acpHandshakeTimeout = 30 * time.Second

// ACPProtocol implements Protocol for the Agent Client Protocol (JSON-RPC 2.0).
type ACPProtocol struct {
	mu sync.Mutex
	// nextID is Int64 so a long-running connector can never sign-flip; allocID
	// narrows to int for RPCRequest.ID JSON compatibility (64-bit only).
	nextID atomic.Int64
	// sessionID is an atomic.Pointer rather than mu-guarded so WriteMessage /
	// WriteInterrupt / readLoop turn-boundary reads never contend with the
	// per-chunk textBuf writes mu serialises. Init stores it once per instance.
	sessionID atomic.Pointer[string]
	// textBuf accumulates agent_message_chunk text during a turn. Guarded by
	// mu: mutated from readLoop, reset from WriteMessage / at turn boundary.
	textBuf strings.Builder
	// thoughtBuf accumulates agent_thought_chunk text during a turn. kiro streams
	// reasoning in ~2-char chunks (100s per turn); flushing one "thinking" block
	// at turn boundary avoids flooding EventLog. Guarded by mu like textBuf.
	thoughtBuf strings.Builder
	// BackendID labels metric increments so protocol code stays independent of
	// the cli/backend registry; empty falls back to LabelEmpty (unwired tests).
	BackendID string

	// ctrlMu guards pendingControl; a dedicated mutex so the per-response lookup
	// on the readLoop never contends with the textBuf accumulation under mu.
	ctrlMu sync.Mutex
	// pendingControl maps an in-flight control RPC id (today only
	// session/set_model) to the caller's requestID. ReadEvent's IsResponse
	// branch consults it FIRST and surfaces a match as a Type:"control_ack"
	// Event — otherwise a mid-turn set_model response would falsely close the
	// active turn. Entries are removed on match (a lost response leaks one entry
	// until teardown). docs/rfc/dashboard-model-effort-control.md §4.4.
	pendingControl map[int]string

	// availableModels caches the agent-reported model manifest from session/new
	// / session/load results. Written once in Init, read on arbitrary goroutines
	// by the /api/cli/backends path — hence atomic. dashboard-model-effort-control.md §4.2.
	availableModels atomic.Pointer[[]ModelInfo]
}

func (p *ACPProtocol) Name() string { return "acp" }

// storeSessionID publishes the session id atomically.
func (p *ACPProtocol) storeSessionID(id string) {
	p.sessionID.Store(&id)
}

// loadSessionID returns the published session id, or "" before Init wrote one.
func (p *ACPProtocol) loadSessionID() string {
	if s := p.sessionID.Load(); s != nil {
		return *s
	}
	return ""
}

// Clone returns a fresh ACPProtocol retaining BackendID so metric labelling survives proto.Clone().
func (p *ACPProtocol) Clone() Protocol { return &ACPProtocol{BackendID: p.BackendID} }

func (p *ACPProtocol) BuildArgs(opts SpawnOptions) []string {
	args := []string{"acp"}
	// kiro-cli acp accepts `--model <ID>` to seed the session's model (verified
	// on kiro 2.3.0; omitting it falls back to "auto"). Never pass an empty
	// flag value — kiro rejects the argv.
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	// `--effort <tier>` overrides kiro's configured default so naozhi owns the
	// tier. Verified on 2.16.0 for BOTH session/new and session/load — a resume
	// spawned without the flag silently reverts to kiro's default. Closed set
	// validated at config load; empty means no flag. docs/rfc/kiro-effort-control.md
	if opts.Effort != "" {
		args = append(args, "--effort", opts.Effort)
	}
	// Same ARG_MAX / denied-flag guard as ClaudeProtocol so ACP cannot bypass it.
	args = append(args, capExtraArgsBytes(opts.ExtraArgs)...)
	return args
}

// acpInitParams matches the parameters of the ACP "initialize" RPC. A named
// struct so json marshaling skips the reflect-on-map-of-interface path.
type acpInitParams struct {
	ProtocolVersion    int                   `json:"protocolVersion"`
	ClientCapabilities acpClientCapabilities `json:"clientCapabilities"`
	ClientInfo         acpClientInfo         `json:"clientInfo"`
}

type acpClientCapabilities struct {
	FS       acpFSCapability `json:"fs"`
	Terminal bool            `json:"terminal"`
}

type acpFSCapability struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type acpClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// acpSessionLoadParams matches the parameters of the ACP "session/load" RPC.
// McpServers is REQUIRED (at least an empty array): kiro-cli's serde rejects a
// request missing it, drops the connection and exits 0, surfacing as "cli
// exited during init" and breaking every kiro resume (#2364).
type acpSessionLoadParams struct {
	SessionID  string `json:"sessionId"`
	Cwd        string `json:"cwd"`
	McpServers []any  `json:"mcpServers"`
}

// acpSessionNewParams matches the parameters of the ACP "session/new" RPC.
// McpServers is always empty but must be an explicit array (see acpSessionLoadParams).
type acpSessionNewParams struct {
	Cwd        string `json:"cwd"`
	McpServers []any  `json:"mcpServers"`
}

func (p *ACPProtocol) Init(rw *JSONRW, resumeID string, cwd string) (string, error) {
	initID := p.allocID()
	initReq := RPCRequest{
		JSONRPC: "2.0", ID: initID, Method: "initialize",
		Params: acpInitParams{
			ProtocolVersion: 1,
			ClientCapabilities: acpClientCapabilities{
				FS:       acpFSCapability{ReadTextFile: true, WriteTextFile: true},
				Terminal: true,
			},
			ClientInfo: acpClientInfo{Name: "naozhi", Version: "1.0.0"},
		},
	}
	if err := p.sendAndWaitResponse(rw, initReq); err != nil {
		return "", fmt.Errorf("acp initialize: %w", err)
	}

	// cwd is the session workspace (opts.WorkingDir); fall back to os.TempDir()
	// only when the caller omitted one (tests, startup probe).
	if cwd == "" {
		cwd = os.TempDir()
	}
	if resumeID != "" {
		loadID := p.allocID()
		loadReq := RPCRequest{
			JSONRPC: "2.0", ID: loadID, Method: "session/load",
			Params: acpSessionLoadParams{SessionID: resumeID, Cwd: cwd, McpServers: []any{}},
		}
		// The Msg variant returns the result payload: kiro returns the same models
		// envelope on load as on new, so resume refreshes the manifest cache too.
		resp, err := p.sendAndWaitResponseMsg(rw, loadReq)
		if err != nil {
			return "", fmt.Errorf("acp session/load: %w", err)
		}
		p.captureModels(resp.Result)
		p.storeSessionID(resumeID)
	} else {
		newReq := RPCRequest{
			JSONRPC: "2.0", ID: p.allocID(), Method: "session/new",
			Params: acpSessionNewParams{Cwd: cwd, McpServers: []any{}},
		}
		// Shared helper keeps metric emission and the readUntilResponse contract
		// in lockstep with initialize / session/load.
		resp, err := p.sendAndWaitResponseMsg(rw, newReq)
		if err != nil {
			return "", fmt.Errorf("acp session/new: %w", err)
		}
		var result ACPSessionNewResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return "", fmt.Errorf("acp parse session/new result: %w", err)
		}
		p.storeModels(result.Models)
		p.storeSessionID(result.SessionID)
	}

	return p.loadSessionID(), nil
}

// captureModels best-effort parses a session/load result for the models
// envelope; parse failures are ignored (the manifest must never fail Init).
func (p *ACPProtocol) captureModels(result json.RawMessage) {
	var parsed ACPSessionNewResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		return
	}
	p.storeModels(parsed.Models)
}

// storeModels normalises and publishes the agent-reported model manifest.
// Entries without a modelId are dropped; nil / empty envelopes leave the
// previous list in place so a transiently silent agent never wipes a good one.
func (p *ACPProtocol) storeModels(env *ACPModelsEnvelope) {
	if env == nil || len(env.AvailableModels) == 0 {
		return
	}
	models := make([]ModelInfo, 0, len(env.AvailableModels))
	for _, m := range env.AvailableModels {
		if m.ModelID == "" {
			continue
		}
		models = append(models, ModelInfo{ID: m.ModelID, Name: m.Name, Description: m.Description})
	}
	if len(models) == 0 {
		return
	}
	p.availableModels.Store(&models)
}

// AvailableModels returns the manifest captured during Init (nil before the
// handshake / on agents that report none). Shared slice — do not mutate.
func (p *ACPProtocol) AvailableModels() []ModelInfo {
	if v := p.availableModels.Load(); v != nil {
		return *v
	}
	return nil
}

// acpImageSource is the inner "source" object of an ACP image content block;
// a named struct so json.Marshal uses the reflect cache (no per-call boxing).
type acpImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// acpPromptBlock is one content block of an ACP session/prompt "prompt" array;
// text and image variants are encoded in one struct via pointer fields. Text
// is a *string so empty text still produces {"type":"text","text":""}
// instead of being dropped by omitempty.
type acpPromptBlock struct {
	Type   string          `json:"type"`
	Source *acpImageSource `json:"source,omitempty"`
	Text   *string         `json:"text,omitempty"`
}

// acpPromptParams is the typed shape of session/prompt's params (no map boxing).
type acpPromptParams struct {
	SessionID string           `json:"sessionId"`
	Prompt    []acpPromptBlock `json:"prompt"`
}

func (p *ACPProtocol) WriteMessage(w io.Writer, text string, images []Attachment) error {
	sid := p.loadSessionID()
	p.mu.Lock()
	p.textBuf.Reset()
	p.thoughtBuf.Reset()
	p.mu.Unlock()

	// Typed []acpPromptBlock so the encoding/json reflect cache hits the same
	// concrete shape every call (no per-block map + interface{} boxing).
	hasText := text != ""
	prompt := make([]acpPromptBlock, 0, len(images)+1)
	for _, img := range images {
		prompt = append(prompt, acpPromptBlock{
			Type: "image",
			Source: &acpImageSource{
				Type:      "base64",
				MediaType: img.MimeType,
				Data:      encodeImageBase64(img.Data),
			},
		})
	}
	if hasText || len(prompt) == 0 {
		// Non-nil *string so the wire frame keeps the "text" key even for
		// empty text.
		t := text
		prompt = append(prompt, acpPromptBlock{Type: "text", Text: &t})
	}

	id := p.allocID()
	req := RPCRequest{
		JSONRPC: "2.0", ID: id, Method: "session/prompt",
		Params: acpPromptParams{
			SessionID: sid,
			Prompt:    prompt,
		},
	}
	// Pooled encoder; Encode appends the NDJSON trailing '\n' itself.
	eb := acpEncPool.Get().(*acpEncBuf)
	defer putACPEncBuf(eb)
	eb.buf.Reset()
	if err := eb.enc.Encode(req); err != nil {
		return err
	}
	_, err := w.Write(eb.buf.Bytes())
	return err
}

// WriteInterrupt sends a session/cancel notification (no id) to abort the
// in-flight session/prompt (verified against kiro 2.3.0,
// docs/rfc/multi-backend-validation.md V1): it must be a NOTIFICATION — as a
// request kiro answers "Method not found"; the prompt RPC then completes with
// stopReason "cancelled" within ms, which readLoop treats as a normal
// turn-end; a few in-flight chunks may still arrive harmlessly. Returns
// ErrInterruptUnsupported before a session exists (callers fall back to SIGINT).
func (p *ACPProtocol) WriteInterrupt(w io.Writer, _ string) error {
	sid := p.loadSessionID()
	if sid == "" {
		return ErrInterruptUnsupported
	}
	// Static envelope + json.Marshal of sid only: the plain-string fast path
	// yields a properly escaped, quoted JSON string with no struct reflection.
	sidJSON, err := json.Marshal(sid)
	if err != nil {
		return fmt.Errorf("acp marshal session/cancel: %w", err)
	}
	var buf [256]byte
	out := buf[:0]
	out = append(out, `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":`...)
	out = append(out, sidJSON...)
	out = append(out, "}}\n"...)
	if _, err := w.Write(out); err != nil {
		return fmt.Errorf("acp write session/cancel: %w", err)
	}
	// Record only successful sends; the pre-handshake early return is not a
	// real cancel reaching the agent.
	metrics.RecordACPCancel(p.BackendID)
	return nil
}

// WriteUserMessageLocked ignores uuid and priority — ACP has neither concept,
// so such sessions fall back to Collect mode regardless of queue.mode.
func (p *ACPProtocol) WriteUserMessageLocked(w io.Writer, _, text string, images []Attachment, _ string) error {
	return p.WriteMessage(w, text, images)
}

// registerPendingControl records an in-flight control RPC id → caller
// requestID mapping for ReadEvent's response interception.
func (p *ACPProtocol) registerPendingControl(id int, requestID string) {
	p.ctrlMu.Lock()
	if p.pendingControl == nil {
		p.pendingControl = make(map[int]string, 1)
	}
	p.pendingControl[id] = requestID
	p.ctrlMu.Unlock()
}

// takePendingControl removes and returns the caller requestID for a control
// RPC id; ("", false) means it is the session/prompt response (normal turn-end).
func (p *ACPProtocol) takePendingControl(id int) (string, bool) {
	p.ctrlMu.Lock()
	defer p.ctrlMu.Unlock()
	reqID, ok := p.pendingControl[id]
	if ok {
		delete(p.pendingControl, id)
	}
	return reqID, ok
}

// dropPendingControl removes a registration after a failed write (the
// request never reached the agent, so no response will arrive).
func (p *ACPProtocol) dropPendingControl(id int) {
	p.ctrlMu.Lock()
	delete(p.pendingControl, id)
	p.ctrlMu.Unlock()
}

// WriteSetModel sends a session/set_model RPC to switch the live session's
// model (verified on kiro 2.20.2, docs/rfc/dashboard-model-effort-control.md
// §1): it succeeds mid-turn and applies from the next turn; kiro validates
// NOTHING — an unknown modelId returns success and fails on the next prompt,
// so callers validate against availableModels first; the switch is
// process-bound, so callers re-apply --model on respawn. ReadEvent intercepts
// the response via pendingControl (see ModelSetter). Returns
// ErrSetModelUnsupported before the handshake.
func (p *ACPProtocol) WriteSetModel(w io.Writer, requestID, model string) error {
	sid := p.loadSessionID()
	if sid == "" {
		return ErrSetModelUnsupported
	}
	id := p.allocID()
	req := RPCRequest{
		JSONRPC: "2.0", ID: id, Method: "session/set_model",
		Params: acpSetModelParams{SessionID: sid, ModelID: model},
	}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("acp marshal session/set_model: %w", err)
	}
	// Register BEFORE the write: the readLoop may deliver the response on
	// another goroutine the instant the frame reaches the agent.
	p.registerPendingControl(id, requestID)
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		p.dropPendingControl(id)
		return fmt.Errorf("acp write session/set_model: %w", err)
	}
	return nil
}

// acpSetModelParams matches the parameters of the ACP "session/set_model"
// RPC (F1). Named struct for the same reflect-cache reason as acpInitParams.
type acpSetModelParams struct {
	SessionID string `json:"sessionId"`
	ModelID   string `json:"modelId"`
}

func (p *ACPProtocol) SupportsPriority() bool { return false }
func (p *ACPProtocol) SupportsReplay() bool   { return false }

// Capabilities returns the hard-coded Caps for ACP JSON-RPC. SoftInterrupt=true:
// session/cancel is a safe soft cancel once the handshake completed (before
// that WriteInterrupt returns ErrInterruptUnsupported → SIGINT fallback).
// EffortTier=true: BuildArgs forwards SpawnOptions.Effort as `--effort`.
func (p *ACPProtocol) Capabilities() Caps {
	return Caps{Replay: false, Priority: false, SoftInterrupt: true, StreamJSON: false,
		EffortTier: true}
}

// ReadEventInto is the allocation-aware variant of ReadEvent (#1676). ACP's
// per-frame shapes vary (zero/one/two events), so the parsed events are copied
// into buf when they fit; only a >cap result falls back to ReadEvent's slice.
func (p *ACPProtocol) ReadEventInto(line string, buf []Event) ([]Event, bool, error) {
	events, done, err := p.ReadEvent(line)
	if err != nil || len(events) == 0 || cap(buf) < len(events) {
		return events, done, err
	}
	return append(buf[:0], events...), done, nil
}

func (p *ACPProtocol) ReadEvent(line string) ([]Event, bool, error) {
	var msg RPCMessage
	// Aliased bytes: json.Unmarshal only reads its input (#700).
	if err := json.Unmarshal(stringToBytesUnsafe(line), &msg); err != nil {
		return nil, false, err
	}

	if msg.IsNotification() && msg.Method == "session/update" {
		ev, done, err := p.parseSessionUpdate(msg.Params)
		if err != nil || ev.Type == "" {
			return nil, done, err
		}
		// Cap total content bytes to bound downstream CPU / memory amplification
		// (EventLog ring, JSONL persist, dashboard fan-out); mirrors ClaudeProtocol.
		if ev.Message != nil {
			if n := contentBytes(ev.Message); n > maxAssistantMessageContentBytes {
				return nil, done, fmt.Errorf("acp: event content exceeds %d bytes (got %d), dropping",
					maxAssistantMessageContentBytes, n)
			}
		}
		return []Event{ev}, done, nil
	}

	// _kiro.dev/metadata is kiro's per-turn status frame (contextUsagePercentage,
	// turnDurationMs, meteringUsage), surfaced as a synthetic Type:"metadata"
	// Event so Process can update SessionView. docs/rfc/multi-backend.md §8.8.
	if msg.IsNotification() && msg.Method == "_kiro.dev/metadata" {
		ev, done, err := parseKiroMetadata(msg.Params)
		if err != nil || ev.Type == "" {
			return nil, done, err
		}
		return []Event{ev}, done, nil
	}

	// session/request_permission: IDAsString tolerates kiro's UUID strings as
	// well as numeric ids (HandleEvent echoes the id verbatim); RawParams carries
	// options[] so HandleEvent can pick the optionId by kind, not by vendor name.
	if msg.IsRequest() && msg.Method == "session/request_permission" {
		ev := Event{Type: "permission_request", RawParams: msg.Params}
		if id, ok := msg.IDAsString(); ok {
			ev.RPCRequestID = id
		}
		return []Event{ev}, false, nil
	}

	if msg.IsResponse() {
		// Control-RPC interception (docs/rfc/dashboard-model-effort-control.md
		// §4.4): a response matching an in-flight session/set_model id is an ack,
		// NOT the session/prompt turn-end, and must be routed out BEFORE the
		// turn-end handling below or a mid-turn set_model would flush textBuf and
		// truncate the reply. Checked for both success and error shapes.
		if id, ok := msg.IDAsInt(); ok {
			if reqID, pending := p.takePendingControl(id); pending {
				ev := Event{Type: "control_ack", SubType: "success", RPCRequestID: reqID}
				if msg.Error != nil {
					ev.SubType = "error"
					ev.Result = osutil.SanitizeForLog(msg.Error.Message, 256)
				}
				return []Event{ev}, false, nil
			}
		}
		if msg.Error != nil {
			// Method on a Response is unknown without caller-side correlation, so
			// pass "" and let operators read the vector by code+backend.
			metrics.RecordProtocolRPCError(p.BackendID, "", strconv.Itoa(msg.Error.Code))
			// msg.Error.Message crosses a trust boundary (the ACP agent) and flows
			// into slog attrs + the dashboard, so control chars / bidi are scrubbed.
			// done=true: an error response to session/prompt closes that turn from
			// kiro's POV; done=false would leave the session stuck in state=running.
			// readLoop turns ErrACPRPC into a synthetic result event.
			return nil, true, fmt.Errorf("%w %d: %s", ErrACPRPC,
				msg.Error.Code, osutil.SanitizeForLog(msg.Error.Message, 256))
		}

		// Decode the optional stopReason ("end_turn", "cancelled", "max_tokens",
		// "tool_use_failure", "refusal") into SubType. kiro commonly returns `null`
		// or `{}`, so a length check + bytes.Contains skips a pointless Unmarshal.
		var stop struct {
			StopReason string `json:"stopReason"`
		}
		if len(msg.Result) >= len(acpStopReasonKey) && bytes.Contains(msg.Result, acpStopReasonKey) {
			_ = json.Unmarshal(msg.Result, &stop) // best-effort; missing => empty
		}

		p.mu.Lock()
		text := p.textBuf.String()
		p.textBuf.Reset()
		thought := p.thoughtBuf.String()
		p.thoughtBuf.Reset()
		p.mu.Unlock()
		// Read sid after releasing mu so the critical section stays textBuf-only.
		sid := p.loadSessionID()

		// Turn boundary emits up to THREE events: an optional "thinking" frame
		// (thoughtBuf; per-chunk rows would flood EventLog), an assistant "text"
		// frame — the ONLY place the visible reply materialises, since chunks only
		// feed textBuf — and a pure result event. Result still carries the text
		// for SendResult.Text, but EventLog treats result as turn metadata only.
		var events []Event
		if thought != "" {
			events = append(events, Event{
				Type:      "assistant",
				SessionID: sid,
				Message: &AssistantMessage{
					Content: []ContentBlock{{Type: "thinking", Text: thought}},
				},
			})
		}
		if text != "" {
			events = append(events, Event{
				Type:      "assistant",
				SessionID: sid,
				Message: &AssistantMessage{
					Content: []ContentBlock{{Type: "text", Text: text}},
				},
			})
		}
		events = append(events, Event{
			Type:      "result",
			SubType:   stop.StopReason,
			Result:    text,
			SessionID: sid,
		})
		return events, true, nil
	}

	return nil, false, nil
}

// permissionResponse is the JSON-RPC response to session/request_permission.
// ID is json.RawMessage so the agent-supplied id (UUID string for kiro, int
// elsewhere) round-trips verbatim — the spec requires an exact match, type included.
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

// pickAllowOptionID returns the optionId of the first allow_* kind in opts, or
// "" when none match. The id must be read from the request because vendors
// differ (kiro emits allow_once / allow_always; older ACP drafts hyphenated
// forms) — hardcoding either breaks the other silently.
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
	// Pick optionId from the request's options[]; fall back to "allow_once" when
	// parsing fails — better to guess than to leave a permission_request
	// unanswered, which stalls the turn indefinitely.
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

	// An empty id (malformed agent request) echoes json null so the JSON-RPC
	// shape is at least syntactically honored. A string that parses via Atoi is
	// already a valid JSON number literal, so reuse it verbatim (no alloc);
	// otherwise json.Marshal keeps string ids quoted and escaped.
	idRaw := json.RawMessage(`null`)
	if ev.RPCRequestID != "" {
		if _, err := strconv.Atoi(ev.RPCRequestID); err == nil {
			idRaw = json.RawMessage(ev.RPCRequestID)
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
	// Pooled encoder shared with WriteMessage.
	eb := acpEncPool.Get().(*acpEncBuf)
	defer putACPEncBuf(eb)
	eb.buf.Reset()
	if err := eb.enc.Encode(resp); err != nil {
		slog.Warn("acp: failed to marshal permission response", "err", err)
		return true
	}
	if _, err := w.Write(eb.buf.Bytes()); err != nil {
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
			// Log so an upstream schema drift is diagnosable without reproducing
			// the session; otherwise the user sees an empty reply and no trail.
			slog.Warn("acp: agent_message_chunk content unmarshal failed",
				"err", err,
				"raw_len", len(update.Update.Content))
		} else if content.Text != "" {
			p.mu.Lock()
			// Cap the streaming buffer at the finalised-message ceiling: without it
			// a runaway ACP peer can stream chunks indefinitely before the turn-end
			// contentBytes check runs and OOM the process. Truncate silently; the
			// downstream guard surfaces the size to logs.
			if room := maxAssistantMessageContentBytes - p.textBuf.Len(); room > 0 {
				if len(content.Text) <= room {
					p.textBuf.WriteString(content.Text)
				} else {
					// Rune-boundary split so a CJK / emoji codepoint straddling room
					// never leaves invalid UTF-8 (bytes are forwarded verbatim to renderers).
					n := textutil.TruncateAtRuneBoundary(content.Text, room)
					p.textBuf.WriteString(content.Text[:n])
				}
			}
			p.mu.Unlock()
		}
		return Event{Type: "assistant", SessionID: update.SessionID}, false, nil

	case "agent_thought_chunk":
		// Reasoning stream: accumulate into thoughtBuf and flush one "thinking"
		// block at turn boundary so the dashboard renders it like Claude's thinking.
		var content ACPTextContent
		if err := json.Unmarshal(update.Update.Content, &content); err != nil {
			slog.Warn("acp: agent_thought_chunk content unmarshal failed",
				"err", err,
				"raw_len", len(update.Update.Content))
		} else if content.Text != "" {
			p.mu.Lock()
			if room := maxAssistantMessageContentBytes - p.thoughtBuf.Len(); room > 0 {
				if len(content.Text) <= room {
					p.thoughtBuf.WriteString(content.Text)
				} else {
					n := textutil.TruncateAtRuneBoundary(content.Text, room)
					p.thoughtBuf.WriteString(content.Text[:n])
				}
			}
			p.mu.Unlock()
		}
		return Event{Type: "assistant", SessionID: update.SessionID}, false, nil

	case "tool_call":
		// Initial invocation. Status defaults to "" ("pending" on the dashboard);
		// tool_call_update events thread by ID and may set completed / failed.
		// Label fields are sanitized: kiro is across the trust boundary and they
		// render directly into chip text/colour.
		sanitizedTitle := sanitizeToolCallLabel(update.Update.Title)
		return Event{
			Type:      "assistant",
			SubType:   "tool_use",
			SessionID: update.SessionID,
			ToolUseID: update.Update.ToolCallID,
			ToolCall: &clievent.ToolCall{
				ID:        update.Update.ToolCallID,
				Title:     sanitizedTitle,
				Kind:      sanitizeToolCallLabel(update.Update.Kind),
				Status:    sanitizeToolCallLabel(update.Update.Status),
				InputJSON: truncateToolJSON(update.Update.RawInput),
			},
			Message: &AssistantMessage{
				Content: []ContentBlock{{Type: "tool_use", Name: sanitizedTitle}},
			},
		}, false, nil

	case "tool_call_update":
		return Event{
			Type:      "assistant",
			SubType:   "tool_result",
			SessionID: update.SessionID,
			ToolUseID: update.Update.ToolCallID,
			ToolCall: &clievent.ToolCall{
				ID:         update.Update.ToolCallID,
				Title:      sanitizeToolCallLabel(update.Update.Title),
				Kind:       sanitizeToolCallLabel(update.Update.Kind),
				Status:     sanitizeToolCallLabel(update.Update.Status),
				InputJSON:  truncateToolJSON(update.Update.RawInput),
				OutputJSON: truncateToolJSON(update.Update.RawOutput),
			},
		}, false, nil

	default:
		return Event{Type: "system", SubType: update.Update.SessionUpdate}, false, nil
	}
}

// allocID returns a monotonically increasing RPC id. The int64 → int
// narrowing is deliberate: RPCRequest.ID is `int` for idiomatic JSON
// marshaling and naozhi only ships 64-bit builds, where it is lossless; a
// 32-bit target would truncate and collide ids.
func (p *ACPProtocol) allocID() int {
	return int(p.nextID.Add(1) - 1)
}

func (p *ACPProtocol) sendAndWaitResponse(rw *JSONRW, req RPCRequest) error {
	_, err := p.sendAndWaitResponseMsg(rw, req)
	return err
}

// sendAndWaitResponseMsg writes req then blocks until a matching response
// arrives, returning the parsed RPCMessage. All handshake RPCs go through here
// so they share one metric-emitting code path; callers that don't need the
// payload use sendAndWaitResponse.
func (p *ACPProtocol) sendAndWaitResponseMsg(rw *JSONRW, req RPCRequest) (*RPCMessage, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if err := rw.WriteLine(data); err != nil {
		return nil, err
	}
	resp, err := p.readUntilResponse(rw, req.ID)
	if err != nil {
		// Record at the call site where req.Method is known. code="" always:
		// extracting the JSON-RPC code from ErrACPRPC would mean re-parsing the
		// formatted error string. Method already splits "init failed" from
		// "prompt failed"; transport errors (ErrACPTimeout) are separable by type.
		metrics.RecordProtocolRPCError(p.BackendID, req.Method, "")
	}
	return resp, err
}

// normalizeContextUsage maps kiro's contextUsagePercentage onto 0-100,
// accepting both 0-1 fractions and already-percent inputs (see parseKiroMetadata).
// Negatives floor to 0; values > 100 (real on kiro when context overflows) are
// clamped because the dashboard's bands and bar width cap at 100 anyway.
func normalizeContextUsage(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v <= 1.0 {
		v *= 100
	}
	if v > 100 {
		return 100
	}
	return v
}

// parseKiroMetadata (below) decodes a _kiro.dev/metadata notification into a
// Type:"metadata" Event (contextUsagePercentage, turnDurationMs, meteringUsage,
// effort). kiro emits TWO frames per turn (verified on 2.16.0): an early one
// with contextUsagePercentage + effort, then one at turn end adding
// meteringUsage + turnDurationMs. contextUsagePercentage arrives both as a 0-1
// fraction and as a percentage that may exceed 100 (see normalizeContextUsage).
// Schema drift is log-and-skip (zero Event) so a reshaped payload never breaks readLoop.

// kiroMeteringEntry mirrors one entry of kiro's `meteringUsage` array. Named so
// the encoding/json type-descriptor cache is shared across calls (anonymous
// nested struct types force a fresh reflect lookup per invocation).
type kiroMeteringEntry struct {
	Value      float64 `json:"value"`
	Unit       string  `json:"unit"`
	UnitPlural string  `json:"unitPlural"`
}

// kiroMetadataParams is the named decoder shape for the kiro metadata body.
type kiroMetadataParams struct {
	SessionID              string              `json:"sessionId"`
	ContextUsagePercentage float64             `json:"contextUsagePercentage"`
	TurnDurationMs         int64               `json:"turnDurationMs"`
	MeteringUsage          []kiroMeteringEntry `json:"meteringUsage"`
	// Effort is RawMessage rather than string so a future kiro that reshapes
	// the field (already a nested output_config.effort object in kiro's own
	// settings) cannot fail the whole-struct Unmarshal and discard the frame's
	// context / duration / metering fields with it. See effortFromRaw.
	Effort json.RawMessage `json:"effort"`
}

// effortFromRaw coerces the raw `effort` value to a tier string. Only a JSON
// string is accepted; any other shape yields "" so the rest of the metadata
// frame survives intact (same log-and-skip posture as parseKiroMetadata).
func effortFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		slog.Warn("acp: _kiro.dev/metadata effort is not a string; ignoring the tier "+
			"and keeping the rest of the frame",
			"err", err, "raw", osutil.SanitizeForLog(string(raw), 64))
		return ""
	}
	// The tier is held for the Process lifetime and re-marshalled on every
	// /api/sessions poll; cap it so an anomalous value cannot be retained or
	// amplified. No ellipsis: "…" would corrupt an identifier the dashboard shows.
	return textutil.TruncateRunesNoEllipsis(s, maxEffortRunes)
}

// maxEffortRunes bounds the retained effort tier. kiro's real tiers are
// low/medium/high/xhigh/max (3-6 chars); the headroom absorbs a future longer
// tier name without letting an anomalous process pin an unbounded string.
const maxEffortRunes = 32

func parseKiroMetadata(params json.RawMessage) (Event, bool, error) {
	var raw kiroMetadataParams
	if err := json.Unmarshal(params, &raw); err != nil {
		slog.Warn("acp: _kiro.dev/metadata unmarshal failed",
			"err", err, "raw_len", len(params))
		return Event{}, false, nil
	}
	meta := &EventMetadata{
		ContextUsagePercent: normalizeContextUsage(raw.ContextUsagePercentage),
		TurnDurationMs:      raw.TurnDurationMs,
		Effort:              effortFromRaw(raw.Effort),
	}
	if len(raw.MeteringUsage) > 0 {
		meta.MeteringUsage = make([]MeteringEntry, 0, len(raw.MeteringUsage))
		for _, m := range raw.MeteringUsage {
			meta.MeteringUsage = append(meta.MeteringUsage, MeteringEntry(m))
		}
	}
	return Event{
		Type:      "metadata",
		SessionID: raw.SessionID,
		Metadata:  meta,
	}, false, nil
}

// readUntilResponse reads lines until a JSON-RPC response with the matching ID
// is found, silently consuming notifications. Times out after
// acpHandshakeTimeout so the caller can never deadlock.
func (p *ACPProtocol) readUntilResponse(rw *JSONRW, expectedID int) (*RPCMessage, error) {
	type readResult struct {
		msg *RPCMessage
		err error
	}
	// ch is buffered (cap 1) so a final-frame send from the goroutine after the
	// caller timed out never blocks; done is an atomic.Bool (no per-handshake
	// chan alloc) that the caller sets on abandonment and the goroutine polls
	// between ReadLine calls and inside send.
	ch := make(chan readResult, 1)
	var done atomic.Bool
	// send drops the result when the caller has already timed out; otherwise a
	// slow ACP peer emitting one final frame after handshake timeout would pin
	// this goroutine until the pipe closed, possibly minutes later.
	send := func(r readResult) {
		if done.Load() {
			return
		}
		select {
		case ch <- r:
		default:
			// ch already holds a result or the caller raced timeout; drop (cap is 1).
		}
	}
	go func() {
		for {
			line, eof, err := rw.R.ReadLine()
			if err != nil {
				send(readResult{nil, fmt.Errorf("read ACP response: %w", err)})
				return
			}
			if eof {
				send(readResult{nil, fmt.Errorf("unexpected EOF during ACP init")})
				return
			}
			if len(line) == 0 {
				continue
			}
			var msg RPCMessage
			if err := json.Unmarshal(line, &msg); err != nil {
				continue
			}
			// naozhi-originated requests always use int ids (allocID); msg.ID is
			// RawMessage only to tolerate string ids on agent-originated requests.
			gotID, gotOK := msg.IDAsInt()
			if msg.IsResponse() && gotOK && gotID == expectedID {
				if msg.Error != nil {
					// Sanitize agent-supplied error text before it reaches slog attrs.
					send(readResult{nil, fmt.Errorf("%w %d: %s", ErrACPRPC,
						msg.Error.Code, osutil.SanitizeForLog(msg.Error.Message, 256))})
					return
				}
				send(readResult{&msg, nil})
				return
			}
			// Caller gave up (timeout): stop early rather than parse frames nobody reads.
			if done.Load() {
				return
			}
		}
	}()

	timer := time.NewTimer(acpHandshakeTimeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		done.Store(true)
		return r.msg, r.err
	case <-timer.C:
		done.Store(true)
		// `done` is only polled between ReadLine calls; a reader parked inside
		// bufio.ReadBytes never observes it. For a shim-backed reader, poke the
		// net.Conn read deadline so ReadBytes returns i/o timeout and the
		// goroutine exits instead of lingering for the connection's lifetime.
		if sl, ok := rw.R.(*shimLineReader); ok && sl.proc != nil && sl.proc.shimConn != nil {
			// Pulse then clear the deadline so later shimConn operations are not
			// prematurely cancelled; the reader observing an error is all we need.
			_ = sl.proc.shimConn.SetReadDeadline(time.Now())
			_ = sl.proc.shimConn.SetReadDeadline(time.Time{})
		}
		// Non-shim readers (no SetReadDeadline hook) leak the goroutine until the
		// ACP process pipe closes — known limitation, no fix designed yet.
		return nil, fmt.Errorf("%w (id=%d)", ErrACPTimeout, expectedID)
	}
}
