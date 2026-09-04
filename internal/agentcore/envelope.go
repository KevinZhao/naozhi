package agentcore

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/naozhi/naozhi/internal/limits"
)

// EnvelopeKind discriminates the bootstrap SSE envelope (one `data:` frame
// per event). Wire contract with spike/agentcore/bootstrap sseEvent.
type EnvelopeKind string

const (
	// KindCLI wraps one raw claude stream-json line from the microVM CLI.
	KindCLI EnvelopeKind = "cli"
	// KindBoot carries bootstrap diagnostics (materialize timing, stderr).
	KindBoot EnvelopeKind = "boot"
	// KindExit reports the CLI process exit; the terminal frame of a clean stream.
	KindExit EnvelopeKind = "exit"
	// KindKeepalive keeps the SSE stream non-silent during quiet tool calls
	// (the platform judges idleness by stream silence). Dropped before fan-out.
	KindKeepalive EnvelopeKind = "keepalive"
	// KindMeta carries the microVM execution receipt (image version, peak
	// RSS), emitted once before the exit frame. Unknown to old readers → skipped.
	KindMeta EnvelopeKind = "meta"
)

// MaxEnvelopeLineBytes is the single ceiling for one serialized SSE envelope
// line, shared by the SSE decoder and the cron reader/writer so any line the
// decoder accepts is also readable back (#2083). limits.MaxStreamJSONLine is
// the CLI stdout cap the bootstrap wraps; 64KiB is envelope headroom (#2084).
const MaxEnvelopeLineBytes = limits.MaxStreamJSONLine + (64 << 10)

// Envelope is one decoded SSE frame from the bootstrap handler.
type Envelope struct {
	Kind EnvelopeKind    `json:"kind"`
	Line json.RawMessage `json:"line,omitempty"` // raw stream-json (kind=cli)
	Msg  string          `json:"msg,omitempty"`  // diagnostics (boot/exit)
	Code int             `json:"code,omitempty"` // CLI exit code (kind=exit)
	// ImageVersion / MemoryPeakBytes are populated only on kind=meta.
	ImageVersion    string `json:"image_version,omitempty"`
	MemoryPeakBytes int64  `json:"memory_peak_bytes,omitempty"`
	TS              string `json:"ts"`
}

// resultProbe is the projection of a stream-json `result` event this layer
// needs (classification, final text, cost/duration receipt), decoded once via
// ParseResultLine (#2321). Full event parsing belongs to cli.Protocol.
type resultProbe struct {
	Type       string  `json:"type"`
	Subtype    string  `json:"subtype"`
	IsError    bool    `json:"is_error"`
	Result     string  `json:"result"`
	CostUSD    float64 `json:"total_cost_usd"`
	DurationMS int64   `json:"duration_ms"`
}

// resultTypeMarker gates the Unmarshal: only one result among thousands of lines.
var resultTypeMarker = []byte(`"type":"result"`)

// ParseResultLine decodes a kind=cli line once when it is the stream-json
// result event; ok=false otherwise. Prefer over chaining the facet helpers.
func ParseResultLine(line json.RawMessage) (p resultProbe, ok bool) {
	if len(line) == 0 || !bytes.Contains(line, resultTypeMarker) {
		return resultProbe{}, false
	}
	if err := json.Unmarshal(line, &p); err != nil || p.Type != "result" {
		return resultProbe{}, false
	}
	return p, true
}

// isResultLine reports whether the line is the result event and whether the
// CLI flagged an error: is_error OR an error_* subtype, so a build that
// reports errors via subtype only does not classify a failed run as Success.
func isResultLine(line json.RawMessage) (isResult, isError bool) {
	p, ok := ParseResultLine(line)
	if !ok {
		return false, false
	}
	return true, p.IsError || strings.HasPrefix(p.Subtype, "error")
}

// ResultText extracts the final result text when the line is the result event.
func ResultText(line json.RawMessage) (text string, ok bool) {
	p, ok := ParseResultLine(line)
	if !ok {
		return "", false
	}
	return p.Result, true
}

// ResultMeta is the cost/duration receipt carried by the result event.
type ResultMeta struct {
	CostUSD    float64
	DurationMS int64
}

// ResultMetaOf extracts the cost/duration receipt when the line is the result event.
func ResultMetaOf(line json.RawMessage) (m ResultMeta, ok bool) {
	p, ok := ParseResultLine(line)
	if !ok {
		return ResultMeta{}, false
	}
	return ResultMeta{CostUSD: p.CostUSD, DurationMS: p.DurationMS}, true
}
