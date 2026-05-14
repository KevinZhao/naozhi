package cli

// process_shim_io.go — shim protocol outbound write path.
//
// Moved from process.go (Phase 1 of docs/rfc/process-split.md).
// Zero semantic change; pure file move. See the RFC for the full
// mapping and why pool lifetime primitives (encodeShimMsg /
// returnShimSendEnc) MUST live in the same file.
//
// Related constants still in process.go:
//   - maxStdinLineBytes (shares const block with readloop constants;
//     kept there to minimise Phase 1 diff; reference is package-private
//     so remains accessible here).
//
// Lock ordering invariant preserved:
//   shimWriter.mu -> Process.shimWMu
// Callers that already hold p.shimWMu must NOT go through
// shimWriter.Write — use shimSendLocked instead.

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
