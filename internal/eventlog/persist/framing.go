package persist

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// Framing layout:
//
//	<decimal-length>\n<json-record-of-length-bytes>\n
//
// <decimal-length> is the ASCII decimal byte count of the JSON record
// (excluding the trailing newline). Length-prefix rather than bare JSONL
// because records with inline image data URIs (30-80 KiB) exceed PIPE_BUF,
// so a concurrent reader can observe a torn write; the prefix lets it
// detect a short tail without JSON salvage. Decimal digits keep the file
// inspectable with less/jq.

// maxLengthDigits bounds the length prefix: MaxRecordBytes (4 MiB) needs 7
// digits, 11 leaves headroom while keeping a corrupt prefix detectable.
const maxLengthDigits = 11

// WriteRecordRaw frames an already-marshalled record body (#1206). body
// MUST be valid schema.Record JSON (schema.MarshalRecord output) or the
// written file becomes unreadable.
func WriteRecordRaw(w io.Writer, body []byte) (int64, error) {
	if len(body) == 0 {
		return 0, ErrEmptyBody
	}
	if len(body) > schema.MaxRecordBytes {
		return 0, fmt.Errorf("body size=%d: %w", len(body), schema.ErrRecordTooLarge)
	}
	return writeFramedBody(w, body)
}

// writeFramedBody writes <length>\n<body>\n as four small Writes. Callers
// MUST pass a buffering writer (Persister wraps logFile in *bufio.Writer)
// so they coalesce into one syscall; the single-writer-per-key invariant
// rules out interleaving.
func writeFramedBody(w io.Writer, body []byte) (int64, error) {
	var lenBuf [20]byte
	lenBytes := strconv.AppendInt(lenBuf[:0], int64(len(body)), 10)
	var total int64

	n, err := w.Write(lenBytes)
	total += int64(n)
	if err != nil {
		return total, err
	}
	n, err = w.Write(newline[:])
	total += int64(n)
	if err != nil {
		return total, err
	}
	n, err = w.Write(body)
	total += int64(n)
	if err != nil {
		return total, err
	}
	n, err = w.Write(newline[:])
	total += int64(n)
	return total, err
}

// newline is package-scoped so writeFramedBody never takes the address of
// a local.
var newline = [1]byte{'\n'}

// ReadRecord reads the next framed record from br. Returns (nil, io.EOF)
// at clean end-of-file, (nil, ErrPartialTail) when the tail holds a partial
// record (writer crashed mid-write or reader caught an in-flight write),
// and (nil, ErrMalformedFrame) when the prefix is not 1..maxLengthDigits
// ASCII digits followed by exactly one '\n' or the body is not followed by
// exactly one '\n' (no resync is possible from that position).
func ReadRecord(br *bufio.Reader) (*schema.Record, error) {
	body, _, err := ReadFramedBody(br)
	if err != nil {
		return nil, err
	}
	// UnmarshalRecord copies (json.RawMessage), so the pooled body can be
	// released on both paths.
	rec, err := schema.UnmarshalRecord(body)
	ReleaseFramedBody(body)
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// framedBodyPool reuses the body+trailing-newline buffer ReadFramedBody
// hands out (recovery walks thousands of frames at startup). It stores
// *[]byte so Put does not box a fresh interface each time. Callers MUST
// return the slice via ReleaseFramedBody; UnmarshalRecord copies, so a
// decoded record never aliases the buffer.
var framedBodyPool = sync.Pool{
	New: func() any {
		// 4 KiB covers typical 1-2 KiB records; acquireFramedBuf regrows
		// for larger frames.
		b := make([]byte, 0, 4096)
		return &b
	},
}

// acquireFramedBuf returns a pooled buffer of length n+1 (body + trailing
// newline), regrowing when the pooled capacity is too small.
func acquireFramedBuf(n int) []byte {
	bp := framedBodyPool.Get().(*[]byte)
	want := n + 1
	if cap(*bp) < want {
		*bp = make([]byte, want)
	} else {
		*bp = (*bp)[:want]
	}
	return *bp
}

// ReleaseFramedBody returns a ReadFramedBody slice to the pool. Call it
// exactly once per successful read and do not retain the slice or any
// subslice afterwards. nil and buffers above 1 MiB are dropped so a
// one-off giant record does not pin memory in the pool.
func ReleaseFramedBody(body []byte) {
	if body == nil {
		return
	}
	if cap(body) > 1<<20 {
		return
	}
	full := body[:cap(body)]
	framedBodyPool.Put(&full)
}

// ReadFramedBody returns the raw record JSON bytes plus the total frame
// length consumed from br. Exposed so rotate can splice records without
// re-marshalling. The slice is borrowed from a sync.Pool: the caller MUST
// call ReleaseFramedBody once done and MUST copy anything it retains past
// the next ReadFramedBody call.
func ReadFramedBody(br *bufio.Reader) ([]byte, int, error) {
	lenBytes, err := br.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			// Bytes before EOF mean a partial length prefix.
			if len(lenBytes) == 0 {
				return nil, 0, io.EOF
			}
			return nil, 0, ErrPartialTail
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			// Length prefix longer than the bufio buffer → malformed.
			return nil, 0, ErrMalformedFrame
		}
		return nil, 0, fmt.Errorf("read length prefix: %w", err)
	}
	digits := lenBytes[:len(lenBytes)-1]
	if len(digits) == 0 || len(digits) > maxLengthDigits {
		return nil, 0, ErrMalformedFrame
	}
	// Byte-level parse avoids strconv.Atoi's bytes→string copy on the
	// recovery path.
	n := 0
	for _, b := range digits {
		if b < '0' || b > '9' {
			return nil, 0, ErrMalformedFrame
		}
		n = n*10 + int(b-'0')
	}
	if n <= 0 {
		return nil, 0, ErrMalformedFrame
	}
	if n > schema.MaxRecordBytes {
		return nil, 0, fmt.Errorf("frame length=%d exceeds cap: %w",
			n, schema.ErrRecordTooLarge)
	}

	// Short read → ErrPartialTail (writer never finished this record). The
	// buffer is pooled: every error path releases it eagerly so a
	// malformed-frame storm cannot drain the pool; success hands ownership
	// to the caller.
	body := acquireFramedBuf(n)
	if _, err := io.ReadFull(br, body); err != nil {
		ReleaseFramedBody(body)
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, 0, ErrPartialTail
		}
		return nil, 0, fmt.Errorf("read body: %w", err)
	}
	if body[n] != '\n' {
		// No trailing newline: the next frame is unreachable, treat the file
		// as truncated here. Release before returning nil (the caller never
		// receives the slice, so no double-free).
		ReleaseFramedBody(body)
		return nil, 0, ErrMalformedFrame
	}

	totalFrame := len(digits) + 1 + n + 1
	return body[:n], totalFrame, nil
}

// Errors surfaced by the framing layer.
var (
	ErrPartialTail    = errors.New("persist: partial record at file tail")
	ErrMalformedFrame = errors.New("persist: malformed frame")
	ErrEmptyBody      = errors.New("persist: empty record body")
)
