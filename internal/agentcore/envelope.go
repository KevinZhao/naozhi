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

// MaxEnvelopeLineBytes is the single-source-of-truth ceiling for one
// serialized SSE envelope line on the sandbox event wire. It bounds BOTH
// ends of the same NDJSON contract so they cannot drift:
//
//   - the SSE decoder (holdStream) sizes its bufio.Scanner token limit to
//     this value — the bootstrap caps each CLI stdout line at 16MB and wraps
//     it in the envelope, so a max-size CLI line still fits with margin;
//   - the cron reader (SandboxRunEvents) and its write-side guard
//     (sandboxEventSink) use the same value, so any line the decoder accepts
//     is also readable back.
//
// R20260613-214326-ARCH-1 (#2083): a previous split (16MB writer / 1MB
// reader) let 1–16MB tool-result lines write but never read — the reader's
// scanner hit bufio.ErrTooLong and silently dropped that line plus every
// later event. Reader-side memory is bounded by the SandboxRunEvents
// concurrency semaphore, not by shrinking this cap below the writer's.
//
// The base CLI stdout ceiling (limits.MaxStreamJSONLine, 16MiB) plus 64KiB
// of envelope/JSON-escaping overhead headroom. Derived from the shared
// constant rather than re-baking the 16MiB literal, so a future CLI cap
// change is made in one place (#2084).
const MaxEnvelopeLineBytes = limits.MaxStreamJSONLine + (64 << 10)

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

// resultProbe is the full projection of a claude stream-json `result`
// event the agentcore layer needs: classification (type/subtype/is_error),
// the final text, and the cost/duration receipt. R202606h-PERF-002 (#2321):
// previously three call sites (isResultLine / ResultText / ResultMetaOf) each
// re-decoded the SAME result line into three overlapping structs. Folding the
// fields into one probe lets a caller that wants more than one facet decode
// once via ParseResultLine. Full event parsing still belongs to cli.Protocol —
// this probe must never grow beyond agentcore's run-record needs.
type resultProbe struct {
	Type       string  `json:"type"`
	Subtype    string  `json:"subtype"`
	IsError    bool    `json:"is_error"`
	Result     string  `json:"result"`
	CostUSD    float64 `json:"total_cost_usd"`
	DurationMS int64   `json:"duration_ms"`
}

// resultTypeMarker gates the probe unmarshal: the stream carries thousands
// of non-result cli lines per job (assistant chunks, tool events) and only
// one result; skip the decode unless the marker appears (mirrors the
// ReadEventInto fast-path precedent in protocol_claude.go).
var resultTypeMarker = []byte(`"type":"result"`)

// ParseResultLine decodes a kind=cli envelope line ONCE when it is the
// stream-json result event, returning the full probe. ok=false (and the
// cheap bytes.Contains short-circuit) for every other line. Callers that
// need more than one result facet should use this instead of calling the
// individual ResultText/ResultMetaOf/isResultLine helpers in sequence —
// each of those re-runs the marker scan + json.Unmarshal on the same bytes.
func ParseResultLine(line json.RawMessage) (p resultProbe, ok bool) {
	if len(line) == 0 || !bytes.Contains(line, resultTypeMarker) {
		return resultProbe{}, false
	}
	if err := json.Unmarshal(line, &p); err != nil || p.Type != "result" {
		return resultProbe{}, false
	}
	return p, true
}

// isResultLine reports whether a kind=cli envelope line is the stream-json
// result event, and if so whether the CLI flagged it as an error. Error is
// signalled by is_error OR an error_* subtype — defence against CLI builds
// that report errors via subtype only (a missing is_error field decodes to
// false, which must not silently classify a failed run as Success).
func isResultLine(line json.RawMessage) (isResult, isError bool) {
	p, ok := ParseResultLine(line)
	if !ok {
		return false, false
	}
	return true, p.IsError || strings.HasPrefix(p.Subtype, "error")
}

// ResultText extracts the final result text from a kind=cli envelope line
// when that line is the stream-json result event. ok=false for every other
// line. Consumers (cron run records) get the CLI's last-turn text without
// growing their own stream-json knowledge.
func ResultText(line json.RawMessage) (text string, ok bool) {
	p, ok := ParseResultLine(line)
	if !ok {
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
	p, ok := ParseResultLine(line)
	if !ok {
		return ResultMeta{}, false
	}
	return ResultMeta{CostUSD: p.CostUSD, DurationMS: p.DurationMS}, true
}
