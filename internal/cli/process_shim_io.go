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
		trimmed := string(data[:len(data)-1])
		if err := w.p.shimSend(shimClientMsg{Type: "write", Line: trimmed}); err != nil {
			return 0, err
		}
		return len(data), nil
	}

	// Slow path: fragmented writes, use buffer.
	w.buf.Write(data)
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
			return 0, fmt.Errorf("%w: %d bytes > %d", ErrMessageTooLarge, len(line)-1, maxStdinLineBytes)
		}
		trimmed := string(line[:len(line)-1])
		// A bare "\n" line (e.g. left-over from a previous Write that put
		// back a partial residual starting with newline) yields an empty
		// trimmed string. The shim ignores blank lines but emitting them
		// wastes a round-trip and pollutes the protocol stream.
		if len(trimmed) == 0 {
			continue
		}
		if err := w.p.shimSend(shimClientMsg{Type: "write", Line: trimmed}); err != nil {
			// Same reason as the size-limit branch: the failed line was
			// already consumed, so leaving the remainder in the buffer would
			// produce a corrupted stitched message on retry.
			w.buf.Reset()
			return 0, err
		}
	}
	return len(data), nil
}

// shimClientMsg is the outgoing message format to the shim.
//
// R226-PERF-10 / R225-PERF-8 archive anchor (doc-and-accept):
//
//	The shimWriter.Write fast path above performs `string(data[:n-1])` to
//	convert the trimmed CLI-stdin line into the Line field, which forces a
//	byte→string copy on every stdin write (5–50 lines/s × N sessions).
//	Reviewers proposed switching Line to `[]byte` / `json.RawMessage` to
//	share the caller's backing array. That alternative was evaluated and
//	rejected: shimClientMsg is the wire format for the parent⇄shim NDJSON
//	protocol (see shimSendEnc / shimSend below) and shim peers must
//	json-Marshal each message into a single newline-terminated JSON record.
//	json.RawMessage requires its bytes to already be a *valid JSON value*,
//	but `data` here is arbitrary CLI stdin (a stream-json client message
//	from the dashboard / channel adapter) which may contain unescaped
//	quotes, backslashes, control bytes, and multi-byte UTF-8. The Marshaler
//	must still escape the payload into a JSON string literal — that escape
//	pass walks the bytes once and copies them into the encoder buffer,
//	producing the exact same one-shot allocation we already pay via
//	`string(data[:n-1])` plus the standard `json.Marshal(string)` fast
//	path. Net: switching the field type would not eliminate the copy, would
//	require a custom Marshaler to avoid double-escape, and would push the
//	allocation cost into a less observable layer. The fast path already
//	short-circuits the multi-line slow path via bytes.IndexByte(...,'\n')
//	== -1, so the unavoidable copy is only paid for one-line writes that
//	are about to be JSON-encoded anyway. Status: doc-and-accept,
//	tracked in docs/TODO.md R226-PERF-10 + R225-PERF-8.
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
