package cli

// process_shim_io.go — shim protocol outbound write path.
//
// Owns: shimWriter (the high-level outbound pump) plus the encoder pool
// primitives (encodeShimMsg / returnShimSendEnc) — pool lifetime MUST
// share this file with shimWriter or buffers escape past their owner's
// scope.
//
// Related constants still in process.go: maxStdinLineBytes, grouped with
// the readloop timing knobs because they share a budget category, not
// because of file-locality.
//
// Lock ordering invariant:
//
//	shimWriter.mu -> Process.shimWMu
//
// Callers that already hold p.shimWMu must NOT go through
// shimWriter.Write — use shimSendLocked instead.
//
// R227-ARCH-19: dropped the "Phase 1 of process-split / zero semantic
// change" preamble; refactor is complete, history lives in git log.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"unicode/utf8"
)

// shimWriter wraps shim protocol write commands as an io.Writer.
// Thread-safe: readLoop (HandleEvent) and Send (WriteMessage) may call concurrently.
//
// Lock ordering: shimWriter.mu -> Process.shimWMu.
// Write() holds w.mu, then calls p.shimSend() which acquires p.shimWMu internally.
// Callers that already hold p.shimWMu (e.g. Kill's pre-close write) must NOT
// go through shimWriter.Write — use shimSendLocked directly to avoid a reverse
// lock ordering and potential deadlock.
type shimWriter struct {
	p   *Process
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *shimWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Fast path: buffer is empty and data is a single complete line ending in '\n'.
	// This is the normal path from Protocol.WriteMessage.
	// The embedded-newline guard ensures multi-line data falls through to the
	// slow path which splits on '\n' correctly.
	if w.buf.Len() == 0 && len(data) > 0 && data[len(data)-1] == '\n' &&
		bytes.IndexByte(data[:len(data)-1], '\n') == -1 {
		if len(data)-1 > maxStdinLineBytes {
			return 0, fmt.Errorf("%w: %d bytes > %d", ErrMessageTooLarge, len(data)-1, maxStdinLineBytes)
		}
		// R245-PERF-1: the prior `string(data[:len(data)-1])` heap-copied
		// every frame just so encoding/json could re-walk the same bytes
		// and write them back out as a JSON-quoted string. shimSendLine
		// pre-quotes the bytes directly into a pooled bytes.Buffer via
		// appendJSONStringBytes, eliding the intermediate Go string alloc.
		if err := w.p.shimSendLine(data[:len(data)-1]); err != nil {
			return 0, err
		}
		return len(data), nil
	}

	// Slow path: fragmented writes, use buffer.
	w.buf.Write(data)
	// io.Writer contract: when returning a non-nil error, n must reflect
	// the count of input bytes already accepted by the writer. Tracking
	// consumed bytes prevents callers from re-sending lines that the shim
	// already received when an error fires partway through a multi-line
	// Write.
	consumed := 0
	for {
		line, err := w.buf.ReadBytes('\n')
		if err != nil {
			// No newline yet — put the partial data back
			w.buf.Write(line)
			break
		}
		// ReadBytes guarantees len(line) >= 1 when err == nil (line ends in '\n'),
		// but stay defensive: a zero-length line would panic on the slice below.
		if len(line) == 0 {
			continue
		}
		if len(line)-1 > maxStdinLineBytes {
			// The offending line was already consumed from w.buf by ReadBytes
			// above; discard any trailing partial lines so the next Write()
			// doesn't concatenate fresh data onto a broken prefix the shim
			// never received.
			w.buf.Reset()
			n := consumed + len(line)
			if n > len(data) {
				n = len(data)
			}
			return n, fmt.Errorf("%w: %d bytes > %d", ErrMessageTooLarge, len(line)-1, maxStdinLineBytes)
		}
		// A bare "\n" line (e.g. left-over from a previous Write that put
		// back a partial residual starting with newline) skips the send
		// path. The shim ignores blank lines but emitting them wastes a
		// round-trip and pollutes the protocol stream.
		if len(line) <= 1 {
			consumed += len(line)
			continue
		}
		// R245-PERF-1: same bytes-direct fast path as above — no
		// intermediate string allocation per buffered line.
		if err := w.p.shimSendLine(line[:len(line)-1]); err != nil {
			// Same reason as the size-limit branch: the failed line was
			// already consumed, so leaving the remainder in the buffer would
			// produce a corrupted stitched message on retry.
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

// shimSendEnc pairs a pooled bytes.Buffer with a json.Encoder bound to it.
// Both are reused across calls so the hot shimSend path has zero encoder
// allocations. The Encoder holds a *bytes.Buffer by pointer, so resetting
// the buffer between uses is safe — the Encoder writes into the same buffer
// on every call.
type shimSendEnc struct {
	buf *bytes.Buffer
	enc *json.Encoder
}

var shimSendBufPool = sync.Pool{
	New: func() any {
		buf := new(bytes.Buffer)
		enc := json.NewEncoder(buf)
		// Shim wire messages carry user content that may contain '<', '>',
		// '&' (code blocks, HTML snippets). The default json.Marshal HTML-
		// escape would deliver `<` style strings to the shim and on to
		// the Claude CLI stdin, subtly mangling payloads.
		enc.SetEscapeHTML(false)
		return &shimSendEnc{buf: buf, enc: enc}
	},
}

// encodeShimMsg marshals msg into a fresh pooled buffer with HTML escaping
// disabled. Caller MUST Put the returned buffer back into shimSendBufPool
// (typically via defer) after the Write+Flush completes.
//
// Encoding outside the write lock keeps shimWMu held only for the length of
// the actual socket write: large messages (e.g. 400KB thumbnails) otherwise
// serialize ping/interrupt on the encoder itself.
func encodeShimMsg(msg shimClientMsg) (*shimSendEnc, error) {
	se := shimSendBufPool.Get().(*shimSendEnc)
	se.buf.Reset()
	// Encoder appends its own trailing '\n' per NDJSON framing, so we must
	// not add one manually.
	if err := se.enc.Encode(msg); err != nil {
		// Do not return this entry to the pool: json.Encoder is not
		// documented to leave clean state after a failed Encode, and
		// buf may hold partial bytes. Let GC reclaim it; the pool's New
		// func will allocate a fresh pair on the next Get.
		return nil, err
	}
	return se, nil
}

// shimSendBufMaxCap caps the buffer capacity we return to the pool. Large
// payloads (e.g. 400KB image paste) grow the underlying bytes.Buffer and
// sync.Pool will not trim it; once a few big messages have passed through,
// pooled entries would permanently hold large backing arrays. Entries that
// exceed this cap are dropped so GC reclaims them; the pool's New allocator
// will produce a fresh small buffer on the next Get.
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

// shimWriteLineFramePrefix / shimWriteLineFrameSuffix bracket the per-frame
// "write" envelope built by shimSendLine. The line payload is JSON-string-
// quoted between them via appendJSONStringBytes; the per-frame state is
// just the (pooled) bytes.Buffer growth, no Go string heap copy.
var (
	shimWriteLineFramePrefix = []byte(`{"type":"write","line":`)
	shimWriteLineFrameSuffix = []byte("}\n")
)

// shimSendLine writes a "write" frame whose line field is the given bytes.
// Equivalent on the wire to shimSend(shimClientMsg{Type: "write", Line:
// string(line)}) but skips the per-frame string(line) heap copy: the bytes
// are JSON-string-escaped directly via appendJSONStringBytes into a pooled
// bytes.Buffer.
//
// R245-PERF-1 (REPEAT-N R71-PERF-H1): shimWriter sees one frame per
// Protocol.WriteMessage call (and one per buffered slow-path line). The
// previous code allocated the trimmed Go string just to hand it to
// encoding/json so json could re-walk the same bytes and write them back
// out as a JSON-quoted string. Pre-quoting into a pooled scratch drops one
// alloc per frame on the hot send path.
func (p *Process) shimSendLine(line []byte) error {
	bp := shimSendBufPool.Get().(*shimSendEnc)
	defer returnShimSendEnc(bp)
	bp.buf.Reset()
	// Build the frame directly into bp.buf using AvailableBuffer() to avoid
	// a separate tmp allocation. Write prefix first so bp.buf grows to
	// accommodate the frame, then borrow its spare capacity for the quoted
	// portion, then append the suffix.
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

// appendJSONStringBytes appends a JSON-encoded string literal of `s` to
// `dst` and returns the extended slice. Mirrors the escape rules
// encoding/json applies to a string field with SetEscapeHTML(false):
// only `"`, `\`, and C0 control bytes are escaped; '<','>','&' pass
// through verbatim. U+2028 / U+2029 are escaped as   /
// (matches stdlib JS-compat behaviour). Invalid UTF-8 bytes become
// � to keep the wire 7-bit clean.
//
// This avoids the round-trip through `string(s)` + json.Marshal that
// shimWriter previously paid per frame — the bytes are walked once and
// quoted directly into the destination buffer.
func appendJSONStringBytes(dst, s []byte) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		b := s[i]
		// Fast path for ASCII printable bytes that need no escaping.
		// Mirrors encoding/json's safeSet: 0x20–0x7E except '"' and '\\'.
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
		// Multibyte UTF-8: validate via utf8.DecodeRune. Invalid sequences
		// become � so the wire stays 7-bit clean.
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
		// encoding/json escapes U+2028 / U+2029 even with SetEscapeHTML
		// false (JS-compat carve-out). Match byte-for-byte.
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

// shimPingBytes is the pre-marshalled NDJSON frame for heartbeat pings.
// The heartbeat loop fires every 30s for every live process, so the
// payload is fully static — there's no runtime field to fill in. Using
// a package-level constant skips the encodeShimMsg pool acquire +
// json.Encoder reflection on the hot path. R222-PERF-14.
//
// The trailing '\n' is mandatory: shim NDJSON framing uses '\n' as the
// record separator, identical to what json.Encoder.Encode appends.
var shimPingBytes = []byte(`{"type":"ping"}` + "\n")

// shimSendRaw writes a pre-marshalled shim wire frame without going
// through encodeShimMsg. The caller MUST guarantee data is a valid
// NDJSON record (typically a package-level constant). R222-PERF-14.
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
