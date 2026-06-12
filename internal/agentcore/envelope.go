package agentcore

import (
	"bytes"
	"encoding/json"
	"strings"
)

// EnvelopeKind discriminates the bootstrap SSE envelope (one `data:` frame
// per event). Wire contract with spike/agentcore/bootstrap sseEvent.
type EnvelopeKind string

const (
	// KindCLI wraps one raw claude stream-json line from the microVM CLI.
	KindCLI EnvelopeKind = "cli"
	// KindBoot carries bootstrap diagnostics (materialize timing, stderr).
	KindBoot EnvelopeKind = "boot"
	// KindExit reports the CLI process exit (code + reason); the terminal
	// frame of a clean stream.
	KindExit EnvelopeKind = "exit"
	// KindKeepalive keeps the SSE stream non-silent during long quiet tool
	// calls — validation F6: the platform judges idleness by stream silence,
	// not process liveness. Keepalives are dropped before event fan-out.
	KindKeepalive EnvelopeKind = "keepalive"
	// KindMeta carries microVM execution receipt fields the CLI stream
	// itself cannot supply: the baked image version and the process peak
	// RSS (RFC §5.1/§7.3 run-record meta). Emitted once by the bootstrap
	// just before the exit frame. Additive: pre-Phase-2 readers skip the
	// unknown kind, so an old cron talking to a new image is unaffected
	// and a new cron talking to an old image simply gets zero meta.
	KindMeta EnvelopeKind = "meta"
)

// Envelope is one decoded SSE frame from the bootstrap handler.
type Envelope struct {
	Kind EnvelopeKind    `json:"kind"`
	Line json.RawMessage `json:"line,omitempty"` // raw stream-json (kind=cli)
	Msg  string          `json:"msg,omitempty"`  // diagnostics (boot/exit)
	Code int             `json:"code,omitempty"` // CLI exit code (kind=exit)
	// ImageVersion / MemoryPeakBytes are populated only on kind=meta
	// (bootstrap execution receipt). omitempty so non-meta frames stay
	// byte-identical to the pre-Phase-2 wire shape.
	ImageVersion    string `json:"image_version,omitempty"`
	MemoryPeakBytes int64  `json:"memory_peak_bytes,omitempty"`
	TS              string `json:"ts"`
}

// resultProbe is the minimal projection of a claude stream-json `result`
// event needed for terminal-state classification (RFC §6.1). Full event
// parsing belongs to cli.Protocol — this probe must never grow beyond
// classification needs.
type resultProbe struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
}

// resultTypeMarker gates the probe unmarshal: the stream carries thousands
// of non-result cli lines per job (assistant chunks, tool events) and only
// one result; skip the decode unless the marker appears (mirrors the
// ReadEventInto fast-path precedent in protocol_claude.go).
var resultTypeMarker = []byte(`"type":"result"`)

// isResultLine reports whether a kind=cli envelope line is the stream-json
// result event, and if so whether the CLI flagged it as an error. Error is
// signalled by is_error OR an error_* subtype — defence against CLI builds
// that report errors via subtype only (a missing is_error field decodes to
// false, which must not silently classify a failed run as Success).
func isResultLine(line json.RawMessage) (isResult, isError bool) {
	if len(line) == 0 || !bytes.Contains(line, resultTypeMarker) {
		return false, false
	}
	var p resultProbe
	if err := json.Unmarshal(line, &p); err != nil {
		return false, false
	}
	if p.Type != "result" {
		return false, false
	}
	return true, p.IsError || strings.HasPrefix(p.Subtype, "error")
}

// ResultText extracts the final result text from a kind=cli envelope line
// when that line is the stream-json result event. ok=false for every other
// line. Consumers (cron run records) get the CLI's last-turn text without
// growing their own stream-json knowledge.
func ResultText(line json.RawMessage) (text string, ok bool) {
	if len(line) == 0 || !bytes.Contains(line, resultTypeMarker) {
		return "", false
	}
	var p struct {
		Type   string `json:"type"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal(line, &p); err != nil || p.Type != "result" {
		return "", false
	}
	return p.Result, true
}

// ResultMeta is the cost/duration receipt carried by the claude stream-json
// result event (RFC §7.3/§7.5/§10). The CLI emits total_cost_usd and the
// run wall-clock; the sandbox run record surfaces them per-run.
type ResultMeta struct {
	CostUSD    float64
	DurationMS int64
}

// ResultMetaOf extracts the cost/duration receipt from a kind=cli envelope
// line when that line is the stream-json result event. Same marker-gated
// fast path as ResultText so the thousands of non-result lines per run pay
// only a bytes.Contains. ok=false for every other line.
func ResultMetaOf(line json.RawMessage) (m ResultMeta, ok bool) {
	if len(line) == 0 || !bytes.Contains(line, resultTypeMarker) {
		return ResultMeta{}, false
	}
	var p struct {
		Type       string  `json:"type"`
		CostUSD    float64 `json:"total_cost_usd"`
		DurationMS int64   `json:"duration_ms"`
	}
	if err := json.Unmarshal(line, &p); err != nil || p.Type != "result" {
		return ResultMeta{}, false
	}
	return ResultMeta{CostUSD: p.CostUSD, DurationMS: p.DurationMS}, true
}
