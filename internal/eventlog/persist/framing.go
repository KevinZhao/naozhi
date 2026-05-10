package persist

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// Framing layout (see RFC §3.1.1):
//
//	<decimal-length>\n<json-record-of-length-bytes>\n
//
// Where <decimal-length> is the ASCII decimal byte count of the JSON
// record (not counting the trailing newline). Example:
//
//	42\n
//	{"v":1,"seq":0,"type":"header","header":{...}}\n
//
// WHY length-prefix instead of bare JSONL:
//
//   - cli.EventEntry records with inline Images data URIs are routinely
//     30-80 KiB, an order of magnitude above POSIX PIPE_BUF (4 KiB).
//     `write(2)` of a buffer larger than PIPE_BUF is NOT guaranteed
//     atomic, so a reader opening the file while the writer is mid-call
//     will see a torn write.
//   - Length-prefix lets the reader detect torn records without parsing
//     JSON: if fewer than `length` bytes follow, it's a partial tail
//     and must be dropped. The reader NEVER attempts JSON-salvage of
//     trailing bytes.
//
// WHY not a fixed-width binary length (uint32 LE etc.):
//
//   - JSONL files are expected to survive `less`/`jq` inspection by
//     operators; a human-readable length prefix is more approachable
//     than a binary header.
//   - 11 decimal digits (up to 99_999_999_999) comfortably exceed
//     MaxRecordBytes, and the cost difference vs 4 binary bytes is
//     negligible.

// maxLengthDigits caps how many ASCII digits we tolerate for a length
// prefix. MaxRecordBytes is 4 MiB (7 digits), so 11 leaves generous
// headroom while still bounding how far the reader has to scan before
// deciding a corrupt length field is fatal.
const maxLengthDigits = 11

// WriteRecord encodes r via schema.MarshalRecord, wraps it in the
// length-prefix framing, and writes the complete frame to w in a single
// Write call.
//
// A single Write() keeps the record intact from the kernel's point of
// view on local ext4 / xfs as long as len(frame) <= 2 GB (no Linux
// write above that returns atomically either way). For writes above
// PIPE_BUF the OS may still split internally, which is precisely why
// the framing protects readers: this function's contract is "emit a
// single logical record", not "be atomic at the syscall layer".
//
// Returns the total number of bytes written (including the prefix
// bytes and the trailing newlines). Callers need this to update
// Persister's per-file byte counter for rotate threshold detection.
func WriteRecord(w io.Writer, r *schema.Record) (int64, error) {
	body, err := schema.MarshalRecord(r)
	if err != nil {
		return 0, err
	}
	return writeFramedBody(w, body)
}

// WriteRecordRaw is the lower-level variant that takes an
// already-marshalled record body. It skips the MarshalRecord call so
// callers (rotate, in particular) don't re-marshal records they're
// just copying from one file to another.
//
// Callers MUST ensure body is a valid schema.Record JSON or the
// written file will be unreadable. Validate + MarshalRecord should be
// the only other path that produces these bytes.
func WriteRecordRaw(w io.Writer, body []byte) (int64, error) {
	if len(body) == 0 {
		return 0, ErrEmptyBody
	}
	if len(body) > schema.MaxRecordBytes {
		return 0, fmt.Errorf("body size=%d: %w", len(body), schema.ErrRecordTooLarge)
	}
	return writeFramedBody(w, body)
}

// writeFramedBody writes the <length>\n<body>\n envelope. The intent
// is for every framed record to land as one logical write whenever the
// OS permits; a pre-sized buffer avoids the two Write calls that would
// otherwise risk interleaving with concurrent writers (though Persister
// is single-writer and this is belt-and-suspenders).
func writeFramedBody(w io.Writer, body []byte) (int64, error) {
	lenStr := strconv.Itoa(len(body))
	// Prefix + '\n' + body + '\n'.
	total := len(lenStr) + 1 + len(body) + 1
	buf := make([]byte, 0, total)
	buf = append(buf, lenStr...)
	buf = append(buf, '\n')
	buf = append(buf, body...)
	buf = append(buf, '\n')
	n, err := w.Write(buf)
	return int64(n), err
}

// FrameSize computes the on-disk length of a framed record given the
// JSON body length. Used by idx entries' Len field — we never write a
// record without knowing its frame size up front, so recomputing on
// the read side would be wasteful.
func FrameSize(bodyLen int) int {
	// Integer log10 of bodyLen for the decimal prefix width, plus two
	// newlines. strconv.Itoa is O(log10) but the alternative is an
	// extra allocation; 20 ns either way is not worth optimizing.
	return len(strconv.Itoa(bodyLen)) + 1 + bodyLen + 1
}

// ReadRecord reads the next framed record from br. Returns (nil, io.EOF)
// at clean end-of-file, (nil, ErrPartialTail) when a partial record is
// detected at the tail (writer crashed mid-write, or reader caught up
// to in-flight write), and (nil, err) for any other decode error.
//
// Callers treating io.EOF and ErrPartialTail identically (readers just
// stop at end of file either way) is fine; the distinction exists so
// tests can assert the exact outcome.
//
// The decoder is strict about the framing:
//
//   - Length prefix must be ASCII digits only, max maxLengthDigits.
//   - Length prefix is followed by exactly one '\n'.
//   - Body is exactly length bytes, followed by exactly one '\n'.
//
// Any deviation → ErrMalformedFrame (non-recoverable for this record
// position — the reader has no way to resync).
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

// ReadFramedBody returns the raw record JSON bytes plus the total
// frame byte length consumed from br. Exposed so the rotate path can
// splice records from old → new file without re-marshalling.
//
// The returned byte slice is a fresh copy (the bufio.Reader's buffer
// may get overwritten on the next read).
func ReadFramedBody(br *bufio.Reader) ([]byte, int, error) {
	// Read length prefix. ReadSlice is fast (no allocation on hit)
	// but its buffer is invalidated by subsequent reads — we copy the
	// digits into lenBuf before continuing.
	lenBytes, err := br.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			// Clean EOF only if the read returned zero bytes; otherwise
			// we consumed a partial prefix before EOF hit.
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
	// lenBytes now ends with '\n'. Slice it off.
	digits := lenBytes[:len(lenBytes)-1]
	if len(digits) == 0 || len(digits) > maxLengthDigits {
		return nil, 0, ErrMalformedFrame
	}
	for _, b := range digits {
		if b < '0' || b > '9' {
			return nil, 0, ErrMalformedFrame
		}
	}
	n, err := strconv.Atoi(string(digits))
	if err != nil || n <= 0 {
		return nil, 0, ErrMalformedFrame
	}
	if n > schema.MaxRecordBytes {
		return nil, 0, fmt.Errorf("frame length=%d exceeds cap: %w",
			n, schema.ErrRecordTooLarge)
	}

	// Read exactly n body bytes + 1 trailing newline. io.ReadFull
	// returns ErrUnexpectedEOF on short read, which maps to "partial
	// tail" here — the writer didn't finish emitting this record.
	body := make([]byte, n+1)
	if _, err := io.ReadFull(br, body); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, 0, ErrPartialTail
		}
		return nil, 0, fmt.Errorf("read body: %w", err)
	}
	if body[n] != '\n' {
		// Missing trailing newline means the next record's framing is
		// unreachable — we can't recover, treat the whole file as
		// truncated at this point.
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
