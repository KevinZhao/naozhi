package cli

// process_shim_io.go — shim protocol outbound write path: shimWriter plus the
// encoder pool primitives (encodeShimMsg / returnShimSendEnc). Pool lifetime
// MUST share this file with shimWriter or buffers escape their owner's scope.
//
// Lock ordering invariant: shimWriter.mu -> Process.shimWMu. Callers that
// already hold p.shimWMu must NOT go through shimWriter.Write — use
// shimSendLocked instead.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"unicode/utf8"
)

// shimWriter wraps shim protocol write commands as an io.Writer. Thread-safe:
// readLoop (HandleEvent) and Send (WriteMessage) may call concurrently. Write()
// holds w.mu then takes p.shimWMu via shimSend — see the file header ordering.
type shimWriter struct {
	p   *Process
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *shimWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Fast path: empty buffer and data is exactly one '\n'-terminated line (the
	// normal Protocol.WriteMessage case); multi-line data takes the slow path.
	if w.buf.Len() == 0 && len(data) > 0 && data[len(data)-1] == '\n' &&
		bytes.IndexByte(data[:len(data)-1], '\n') == -1 {
		if len(data)-1 > maxStdinLineBytes {
			return 0, fmt.Errorf("%w: %d bytes > %d", ErrMessageTooLarge, len(data)-1, maxStdinLineBytes)
		}
		// shimSendLine quotes the bytes directly into a pooled buffer (no string alloc).
		if err := w.p.shimSendLine(data[:len(data)-1]); err != nil {
			return 0, err
		}
		return len(data), nil
	}

	// Slow path: fragmented writes, use buffer.
	w.buf.Write(data)

	// Size-check EVERY complete buffered line BEFORE sending any: sent bytes
	// cannot be un-sent, so rejecting a later oversized line mid-loop would leave
	// a truncated NDJSON frame stream on the wire (#2293).
	if buffered := w.buf.Bytes(); bytes.IndexByte(buffered, '\n') != -1 {
		off := 0
		for off < len(buffered) {
			nl := bytes.IndexByte(buffered[off:], '\n')
			if nl == -1 {
				break // trailing partial line — not yet a complete frame
			}
			lineLen := nl // bytes before '\n'
			if lineLen > maxStdinLineBytes {
				// Discard the whole buffer: the oversized line and trailing partial cannot
				// form a valid frame, and the next Write would stitch onto a broken prefix.
				w.buf.Reset()
				return 0, fmt.Errorf("%w: %d bytes > %d", ErrMessageTooLarge, lineLen, maxStdinLineBytes)
			}
			off += nl + 1
		}
	}

	// Drain the checked lines. A socket failure is unavoidably non-atomic but
	// already tears down the process. io.Writer contract: on error n counts input
	// bytes already accepted, so callers don't re-send lines the shim received.
	consumed := 0
	for {
		line, err := w.buf.ReadBytes('\n')
		if err != nil {
			// No newline yet — put the partial data back
			w.buf.Write(line)
			break
		}
		// ReadBytes guarantees len(line) >= 1 here, but stay defensive.
		if len(line) == 0 {
			continue
		}
		// A bare "\n" (residual from a previous partial Write) skips the send: the
		// shim ignores blank lines but they waste a round-trip.
		if len(line) <= 1 {
			consumed += len(line)
			continue
		}
		if err := w.p.shimSendLine(line[:len(line)-1]); err != nil {
			// The failed line was already consumed; leaving the remainder would stitch
			// a corrupted message on retry.
			w.buf.Reset()
			n := consumed + len(line)
			if n > len(data) {
				n = len(data)
			}
			return n, err
		}
		consumed += len(line)
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

// shimSendEnc pairs a pooled bytes.Buffer with a json.Encoder bound to it so
// the hot shimSend path has zero encoder allocations. The Encoder holds the
// buffer by pointer, so resetting it between uses is safe.
type shimSendEnc struct {
	buf *bytes.Buffer
	enc *json.Encoder
}

var shimSendBufPool = sync.Pool{
	New: func() any {
		buf := new(bytes.Buffer)
		enc := json.NewEncoder(buf)
		// User content carries '<', '>', '&' (code, HTML); the default HTML escape
		// would deliver `<` to the CLI stdin, mangling payloads.
		enc.SetEscapeHTML(false)
		return &shimSendEnc{buf: buf, enc: enc}
	},
}

// encodeShimMsg marshals msg into a pooled buffer with HTML escaping disabled.
// Caller MUST return it via returnShimSendEnc after Write+Flush. Encoding
// outside the write lock keeps shimWMu held only for the socket write, so a
// 400KB thumbnail does not serialize ping/interrupt on the encoder.
func encodeShimMsg(msg shimClientMsg) (*shimSendEnc, error) {
	se := shimSendBufPool.Get().(*shimSendEnc)
	se.buf.Reset()
	// Encode appends the NDJSON trailing '\n' itself.
	if err := se.enc.Encode(msg); err != nil {
		// Don't pool after a failed Encode: the Encoder's state is undocumented and
		// buf may hold partial bytes. Let GC reclaim it.
		return nil, err
	}
	return se, nil
}

// shimSendBufMaxCap caps the buffer capacity returned to the pool: sync.Pool
// never trims, so a few 400KB image pastes would otherwise pin large backing
// arrays forever. Oversized entries are dropped for GC.
const shimSendBufMaxCap = 64 * 1024

func returnShimSendEnc(se *shimSendEnc) {
	if se.buf.Cap() > shimSendBufMaxCap {
		return
	}
	shimSendBufPool.Put(se)
}

func (p *Process) shimSend(msg shimClientMsg) error {
	se, err := encodeShimMsg(msg)
	if err != nil {
		return err
	}
	defer returnShimSendEnc(se)

	p.shimWMu.Lock()
	defer p.shimWMu.Unlock()
	if _, err := p.shimW.Write(se.buf.Bytes()); err != nil {
		return err
	}
	return p.shimW.Flush()
}

// shimWriteLineFramePrefix / shimWriteLineFrameSuffix bracket the "write"
// envelope shimSendLine builds; the line is JSON-quoted between them.
var (
	shimWriteLineFramePrefix = []byte(`{"type":"write","line":`)
	shimWriteLineFrameSuffix = []byte("}\n")
)

// shimSendLine writes a "write" frame whose line field is the given bytes —
// wire-equivalent to shimSend(shimClientMsg{Type: "write", Line: string(line)})
// minus the per-frame string alloc: bytes are quoted straight into a pooled buffer.
func (p *Process) shimSendLine(line []byte) error {
	bp := shimSendBufPool.Get().(*shimSendEnc)
	defer returnShimSendEnc(bp)
	bp.buf.Reset()
	// Write the prefix first so bp.buf grows, then borrow its spare capacity via
	// AvailableBuffer() for the quoted portion — no separate tmp allocation.
	bp.buf.Write(shimWriteLineFramePrefix)
	line2 := bp.buf.AvailableBuffer()
	line2 = appendJSONStringBytes(line2, line)
	bp.buf.Write(line2)
	bp.buf.Write(shimWriteLineFrameSuffix)

	p.shimWMu.Lock()
	defer p.shimWMu.Unlock()
	if _, err := p.shimW.Write(bp.buf.Bytes()); err != nil {
		return err
	}
	return p.shimW.Flush()
}

// appendJSONStringBytes appends a JSON string literal of s to dst, mirroring
// encoding/json with SetEscapeHTML(false): only `"`, `\`, and C0 control bytes
// are escaped; '<','>','&' pass through. U+2028 / U+2029 are escaped as
// \u2028 / \u2029 (stdlib JS-compat behaviour). Invalid UTF-8 bytes become
// \ufffd to keep the wire 7-bit clean.
func appendJSONStringBytes(dst, s []byte) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		b := s[i]
		// ASCII fast path, mirroring encoding/json's safeSet.
		if b < utf8.RuneSelf {
			if b >= 0x20 && b != '"' && b != '\\' {
				i++
				continue
			}
			if start < i {
				dst = append(dst, s[start:i]...)
			}
			switch b {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			default:
				const hex = "0123456789abcdef"
				dst = append(dst, '\\', 'u', '0', '0', hex[b>>4], hex[b&0xF])
			}
			i++
			start = i
			continue
		}
		// Multibyte UTF-8: invalid sequences become \ufffd.
		r, size := utf8.DecodeRune(s[i:])
		if r == utf8.RuneError && size == 1 {
			if start < i {
				dst = append(dst, s[start:i]...)
			}
			dst = append(dst, '\\', 'u', 'f', 'f', 'f', 'd')
			i += size
			start = i
			continue
		}
		// encoding/json escapes U+2028 / U+2029 even with SetEscapeHTML(false).
		if r == 0x2028 || r == 0x2029 {
			if start < i {
				dst = append(dst, s[start:i]...)
			}
			dst = append(dst, '\\', 'u', '2', '0', '2')
			if r == 0x2028 {
				dst = append(dst, '8')
			} else {
				dst = append(dst, '9')
			}
			i += size
			start = i
			continue
		}
		i += size
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	dst = append(dst, '"')
	return dst
}

// shimPingBytes is the pre-marshalled NDJSON heartbeat frame: fully static, so
// a package-level constant skips the pool acquire + json.Encoder reflection
// every 30s per live process. The trailing '\n' is mandatory NDJSON framing.
var shimPingBytes = []byte(`{"type":"ping"}` + "\n")

// shimSendRaw writes a pre-marshalled shim wire frame. The caller MUST
// guarantee data is a valid NDJSON record (typically a package-level constant).
func (p *Process) shimSendRaw(data []byte) error {
	p.shimWMu.Lock()
	defer p.shimWMu.Unlock()
	if _, err := p.shimW.Write(data); err != nil {
		return err
	}
	return p.shimW.Flush()
}

// shimSendLocked is the locked variant of shimSend. The caller MUST hold
// p.shimWMu. Kill() uses this to batch SetWriteDeadline+send+Close under a
// single lock acquisition to avoid racing a concurrent shimSend.
func (p *Process) shimSendLocked(msg shimClientMsg) error {
	se, err := encodeShimMsg(msg)
	if err != nil {
		return err
	}
	defer returnShimSendEnc(se)

	if _, err := p.shimW.Write(se.buf.Bytes()); err != nil {
		return err
	}
	return p.shimW.Flush()
}
