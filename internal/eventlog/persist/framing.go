package persist

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"

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
	rec, err := schema.UnmarshalRecord(body)
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// ReadFramedBody returns the raw record JSON bytes plus the total frame
// length consumed from br. Exposed so rotate can splice records without
// re-marshalling. The returned slice is freshly allocated and owned by the
// caller.
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

	// Short read → ErrPartialTail (writer never finished this record).
	body := make([]byte, n+1)
	if _, err := io.ReadFull(br, body); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, 0, ErrPartialTail
		}
		return nil, 0, fmt.Errorf("read body: %w", err)
	}
	if body[n] != '\n' {
		// No trailing newline: the next frame is unreachable, treat the file
		// as truncated here.
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
