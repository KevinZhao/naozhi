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

// writeFramedBody writes the <length>\n<body>\n envelope as four
// small Writes: digits, '\n', body, '\n'. Callers MUST pass a writer
// that buffers (Persister wraps logFile in *bufio.Writer) so these
// land as a single syscall whenever the bufio buffer has room. The
// single-writer invariant from Persister (one goroutine per key)
// means no interleaving is possible regardless of how many Writes
// the envelope takes.
//
// The earlier implementation allocated a temporary []byte sized to
// the full frame (`make([]byte, 0, total)` + four appends), burning
// ~10 MB/s of heap at 1000 evt/s × 10 KiB records. Because the
// writer is guaranteed single-threaded per key, the "single Write"
// atomicity guarantee the old comment worried about was never
// actually needed — the bufio buffer serializes us just fine, and
// the four tiny Writes coalesce inside bufio at zero cost.
func writeFramedBody(w io.Writer, body []byte) (int64, error) {
	lenStr := strconv.Itoa(len(body))
	var total int64

	// Four Writes, not one: bufio.Writer absorbs them into its
	// internal buffer. A non-bufio writer would still see the frame
	// in order, just as four separate syscalls — but Persister owns
	// the only path here and provides the bufio.
	n, err := io.WriteString(w, lenStr)
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

// newline is a shared single-byte array whose Write into bufio is
// effectively zero-cost (bufio compares the slice against its own
// buffer head — no escape, no alloc). Defined at package scope so
// writeFramedBody doesn't take the address of a local each call.
var newline = [1]byte{'\n'}

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
	// UnmarshalRecord copies into Record fields (json.RawMessage's
	// UnmarshalJSON appends to a fresh slice), so the body buffer can
	// be returned to the pool on both success and failure paths.
	rec, err := schema.UnmarshalRecord(body)
	ReleaseFramedBody(body)
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// framedBodyPool reuses the body+trailing-newline buffer that
// ReadFramedBody allocates per frame. Recovery startup walks every
// record on disk (potentially thousands of frames) and the previous
// implementation paid `make([]byte, n+1)` per frame regardless of how
// short-lived the slice was. R242-PERF-1 (REPEAT-3 with R218-PERF-10).
//
// The pool stores `*[]byte` rather than `[]byte` so the value put back
// is a stable pointer (sync.Pool's New returns interface{}, and a bare
// []byte boxes into a fresh interface allocation on every Put — the
// pointer indirection sidesteps that).
//
// Callers MUST hand the slice back via ReleaseFramedBody once they're
// done extracting / copying / decoding. UnmarshalRecord copies via
// json.RawMessage.UnmarshalJSON so the returned record does NOT retain
// the input slice; callers like ReadRecord and rotate.spliceLog are
// safe to release immediately after the consume step.
var framedBodyPool = sync.Pool{
	New: func() any {
		// Default capacity sized for the common case: most records are
		// 1-2 KiB, large image entries can hit 30-80 KiB. 4 KiB matches
		// the bufio default buffer size and amortises over typical
		// recovery runs without tying up huge backing arrays for runs
		// that never see large frames. Pool grows backing arrays via
		// the n+1 grow path below when a single record exceeds cap.
		b := make([]byte, 0, 4096)
		return &b
	},
}

// acquireFramedBuf fetches a buffer from the pool sized to hold n+1
// bytes (n body + 1 trailing newline). When the pooled buffer's cap is
// too small, we replace it with a freshly grown one — Get returns
// whatever was previously Put, which may have been undersized for the
// next frame.
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

// ReleaseFramedBody returns a slice obtained from ReadFramedBody to
// the internal pool. Callers must call this exactly once per
// ReadFramedBody success and must not retain the slice or any subslice
// after the call (the next reader gets the same backing array).
//
// Passing nil or a slice whose backing array exceeds the pool's
// reasonable max (1 MiB) is silently ignored: huge one-off records
// from a giant image upload shouldn't pin big backing arrays in the
// pool indefinitely.
func ReleaseFramedBody(body []byte) {
	if body == nil {
		return
	}
	// Cap returned buffers at 1 MiB. Beyond this we'd be hoarding
	// memory across the entire process lifetime for a single
	// outlier; let GC reclaim it instead.
	if cap(body) > 1<<20 {
		return
	}
	full := body[:cap(body)]
	framedBodyPool.Put(&full)
}

// ReadFramedBody returns the raw record JSON bytes plus the total
// frame byte length consumed from br. Exposed so the rotate path can
// splice records from old → new file without re-marshalling.
//
// The returned byte slice is borrowed from an internal sync.Pool. The
// caller MUST call ReleaseFramedBody on the returned slice once it has
// finished consuming the bytes (decoded into a record, written to
// another file, etc.). Failing to release simply forfeits the alloc
// savings; passing the same slice twice will not corrupt anything but
// risks two readers handing back overlapping buffers and clobbering
// each other on a future frame.
//
// Callers that intend to retain the body bytes past the next frame
// MUST copy them out — the pool may hand the same backing array to the
// next ReadFramedBody call. Today both production callers (ReadRecord
// and rotate.spliceLog) consume the body synchronously and release.
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
	// Inline byte-level decimal parse — strconv.Atoi(string(digits)) used to
	// force a bytes→string heap copy on every frame, and the recovery path
	// reads thousands of frames at startup. R218-PERF-10. The digit-range
	// check below collapses the validation loop into the parse.
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

	// Read exactly n body bytes + 1 trailing newline. io.ReadFull
	// returns ErrUnexpectedEOF on short read, which maps to "partial
	// tail" here — the writer didn't finish emitting this record.
	//
	// The buffer is borrowed from framedBodyPool. On every error path
	// below we return it eagerly so a malformed-frame storm (e.g. log
	// recovery on a corrupted file) doesn't drain the pool. The success
	// path hands ownership to the caller; ReleaseFramedBody is the
	// matching free.
	body := acquireFramedBuf(n)
	if _, err := io.ReadFull(br, body); err != nil {
		ReleaseFramedBody(body)
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, 0, ErrPartialTail
		}
		return nil, 0, fmt.Errorf("read body: %w", err)
	}
	if body[n] != '\n' {
		// Missing trailing newline means the next record's framing is
		// unreachable — we can't recover, treat the whole file as
		// truncated at this point.
		//
		// R245-PERF-8 follow-up: ReleaseFramedBody is invoked exactly once.
		// An earlier refactor accidentally double-released the same buffer,
		// which violates the pool invariant ("the next reader gets the same
		// backing array") and could hand the same slice to two concurrent
		// ReadFramedBody callers — clobbering each other on the next frame.
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
