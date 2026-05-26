package persist

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// writeRecord is a test-only helper that marshals a schema.Record then
// frames it via writeFramedBody. Production callers always have an
// already-marshalled body and use WriteRecordRaw; this combo step is
// useful for tests building records by value. (DEADCODE-13: previously
// exported as WriteRecord; never had a non-test caller.)
func writeRecord(w io.Writer, r *schema.Record) (int64, error) {
	body, err := schema.MarshalRecord(r)
	if err != nil {
		return 0, err
	}
	return writeFramedBody(w, body)
}

// frameSize computes the on-disk length of a framed record given the
// JSON body length, so TestFrameSize_MatchesWriteRecord can pin the
// invariant against WriteRecordRaw's return value. (DEADCODE-13:
// previously exported as FrameSize; never had a non-test caller.)
func frameSize(bodyLen int) int {
	return len(strconv.Itoa(bodyLen)) + 1 + bodyLen + 1
}

// TestWriteRecord_HappyPath emits a small record and confirms the
// framing shape (<len>\n<json>\n). The size returned must match the
// bytes written — callers feed this into the per-file byte counter
// that drives rotate thresholds, so a drift here causes rotate to
// fire at the wrong time.
func TestWriteRecord_HappyPath(t *testing.T) {
	var buf bytes.Buffer
	r := schema.NewHeader("k", 42, "gen")

	n, err := writeRecord(&buf, r)
	if err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if n != int64(buf.Len()) {
		t.Errorf("returned n=%d, buffer wrote %d — byte counter would drift",
			n, buf.Len())
	}

	framed := buf.Bytes()
	// Find the first newline — that's the length prefix boundary.
	nl := bytes.IndexByte(framed, '\n')
	if nl <= 0 {
		t.Fatalf("no length prefix newline found: %q", framed)
	}
	// Trailing byte must be '\n' (closes the frame).
	if framed[len(framed)-1] != '\n' {
		t.Errorf("frame doesn't end with newline: %q", framed[len(framed)-3:])
	}
	// Length prefix should equal the body size.
	bodyLen := len(framed) - nl - 2 // minus '\n' after length, minus '\n' after body
	wantPrefix := []byte(intToStr(bodyLen))
	if !bytes.Equal(framed[:nl], wantPrefix) {
		t.Errorf("length prefix=%s, want %s", framed[:nl], wantPrefix)
	}
}

// TestReadRecord_RoundTrip is the core happy path: write → read →
// compare. Catches any asymmetry between the two halves of the framing
// protocol.
func TestReadRecord_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := schema.NewEntry(7, []byte(`{"time":1,"uuid":"aa","type":"user","summary":"hi"}`))
	if _, err := writeRecord(&buf, want); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := ReadRecord(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Seq != 7 || got.Type != schema.TypeEntry {
		t.Errorf("unexpected record: %+v", got)
	}
	if !bytes.Equal(got.Entry, want.Entry) {
		t.Errorf("Entry payload mismatch\n got: %q\n want: %q", got.Entry, want.Entry)
	}
}

// TestReadRecord_MultipleInSequence covers the sequencing that startup
// recovery walks — many records in one file, each fully framed.
func TestReadRecord_MultipleInSequence(t *testing.T) {
	var buf bytes.Buffer
	for i := uint64(1); i <= 5; i++ {
		r := schema.NewEntry(i, []byte(`{"time":1,"uuid":"aa","type":"user"}`))
		if _, err := writeRecord(&buf, r); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	br := bufio.NewReader(&buf)
	for i := uint64(1); i <= 5; i++ {
		r, err := ReadRecord(br)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if r.Seq != i {
			t.Errorf("got seq=%d, want %d", r.Seq, i)
		}
	}
	// Next read must be clean EOF — no partial anywhere.
	_, err := ReadRecord(br)
	if !errors.Is(err, io.EOF) {
		t.Errorf("post-last read err=%v, want io.EOF", err)
	}
}

// TestReadRecord_PartialTail_MissingBody simulates "writer finished
// length prefix, crashed before flushing body" — reader sees the
// length line but then EOF immediately. Must return ErrPartialTail so
// recovery can truncate the log at the idx-backed safe edge.
func TestReadRecord_PartialTail_MissingBody(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("100\n"))
	_, err := ReadRecord(br)
	if !errors.Is(err, ErrPartialTail) {
		t.Errorf("err=%v, want ErrPartialTail", err)
	}
}

// TestReadRecord_PartialTail_ShortBody simulates "writer started body
// write, crashed mid-way". io.ReadFull returns ErrUnexpectedEOF which
// maps to ErrPartialTail.
func TestReadRecord_PartialTail_ShortBody(t *testing.T) {
	// Declare 100-byte body, supply only 10.
	data := "100\nabcdefghij"
	br := bufio.NewReader(strings.NewReader(data))
	_, err := ReadRecord(br)
	if !errors.Is(err, ErrPartialTail) {
		t.Errorf("err=%v, want ErrPartialTail", err)
	}
}

// TestReadRecord_MissingTrailingNewline catches the case where a bug
// forgets to emit the final '\n'. The next record's framing is lost,
// so recovery must treat this as truncation, not "still readable".
func TestReadRecord_MissingTrailingNewline(t *testing.T) {
	// 5-byte body "hello" without trailing newline.
	data := "5\nhello"
	br := bufio.NewReader(strings.NewReader(data))
	_, err := ReadRecord(br)
	if !errors.Is(err, ErrPartialTail) {
		t.Errorf("err=%v, want ErrPartialTail", err)
	}
}

// TestReadRecord_MalformedLength covers the "length prefix is garbage"
// path. Each case should fail — what matters is that NONE of them slip
// through silently or panic.
func TestReadRecord_MalformedLength(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"empty prefix", "\n{}\n"},
		{"non-digit", "12a\n{}\n"},
		{"leading space", " 12\n{}\n"},
		{"negative", "-1\n{}\n"},
		{"zero (empty record)", "0\n\n"},
		// A length prefix of 12 digits is > maxLengthDigits (11), which
		// would let us decode a 100 GB "record" if unbounded.
		{"too many digits", "123456789012\n{}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadRecord(bufio.NewReader(strings.NewReader(tc.data)))
			if err == nil {
				t.Fatalf("expected decode error for %q, got nil", tc.data)
			}
			// Accept any of: ErrMalformedFrame, ErrPartialTail. Both
			// are consistent with "stop reading this file" — what we
			// forbid is io.EOF (silent end) or a successful record.
			if errors.Is(err, io.EOF) {
				t.Errorf("got io.EOF, want a frame error for %q", tc.data)
			}
		})
	}
}

// TestReadRecord_LengthExceedsMaxRecord rejects frames that claim a
// body size > MaxRecordBytes. Without the cap a 4 GB length would
// drive the reader to allocate and io.ReadFull N GB.
func TestReadRecord_LengthExceedsMaxRecord(t *testing.T) {
	// MaxRecordBytes + 1.
	data := intToStr(schema.MaxRecordBytes+1) + "\n{}\n"
	_, err := ReadRecord(bufio.NewReader(strings.NewReader(data)))
	if !errors.Is(err, schema.ErrRecordTooLarge) {
		t.Errorf("err=%v, want ErrRecordTooLarge", err)
	}
}

// TestWriteRecordRaw_RejectsEmpty and TestWriteRecordRaw_RejectsOversize
// guard the low-level splice path used by rotate.
func TestWriteRecordRaw_RejectsEmpty(t *testing.T) {
	_, err := WriteRecordRaw(&bytes.Buffer{}, nil)
	if !errors.Is(err, ErrEmptyBody) {
		t.Errorf("err=%v, want ErrEmptyBody", err)
	}
}

func TestWriteRecordRaw_RejectsOversize(t *testing.T) {
	big := make([]byte, schema.MaxRecordBytes+1)
	_, err := WriteRecordRaw(&bytes.Buffer{}, big)
	if !errors.Is(err, schema.ErrRecordTooLarge) {
		t.Errorf("err=%v, want ErrRecordTooLarge", err)
	}
}

// TestFrameSize_MatchesWriteRecord ensures the idx Len field we store
// up front equals the actual bytes WriteRecord emits. A drift here
// causes rotate's cut-offset math to be off by 1+log10(len) per
// record.
func TestFrameSize_MatchesWriteRecord(t *testing.T) {
	bodies := [][]byte{
		[]byte(`{"a":1}`),
		[]byte(`{"a":1,"b":"` + strings.Repeat("x", 1000) + `"}`),
		[]byte(`{"a":1,"b":"` + strings.Repeat("x", 100000) + `"}`),
	}
	for _, body := range bodies {
		var buf bytes.Buffer
		n, err := WriteRecordRaw(&buf, body)
		if err != nil {
			t.Fatalf("write %d bytes: %v", len(body), err)
		}
		want := int64(frameSize(len(body)))
		if n != want {
			t.Errorf("bodyLen=%d: WriteRecordRaw returned %d, frameSize=%d",
				len(body), n, want)
		}
		if int64(buf.Len()) != want {
			t.Errorf("bodyLen=%d: buffer len=%d, frameSize=%d",
				len(body), buf.Len(), want)
		}
	}
}

// TestReadFramedBody_ReturnsTotalFrameLen exposes the second return
// value rotate uses to advance its read cursor when splicing records.
func TestReadFramedBody_ReturnsTotalFrameLen(t *testing.T) {
	var buf bytes.Buffer
	body := []byte(`{"a":1,"b":2}`)
	wantN, _ := WriteRecordRaw(&buf, body)

	gotBody, gotN, err := ReadFramedBody(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(gotBody, body) {
		t.Errorf("body mismatch\n got: %q\n want: %q", gotBody, body)
	}
	if int64(gotN) != wantN {
		t.Errorf("frame size returned %d, write returned %d", gotN, wantN)
	}
}

// TestReadRecord_NonJSONBody covers the "length prefix correct but
// body isn't valid Record JSON" case. Must surface as a decode error,
// not a partial tail (the byte layout was complete).
func TestReadRecord_NonJSONBody(t *testing.T) {
	body := "not json at all"
	data := intToStr(len(body)) + "\n" + body + "\n"
	_, err := ReadRecord(bufio.NewReader(strings.NewReader(data)))
	if err == nil {
		t.Fatal("expected error on non-JSON body")
	}
	if errors.Is(err, ErrPartialTail) || errors.Is(err, io.EOF) {
		t.Errorf("non-JSON body incorrectly mapped to %v", err)
	}
}

// TestReleaseFramedBody_NilSafe verifies the R242-PERF-1 contract:
// callers handing back a nil slice (e.g. early-error path) must not
// panic. The pool's New() returns *[]byte so a nil slice would only
// arrive via deliberate caller misuse, but the guard hardens the API
// against that without forcing every caller to nil-check.
func TestReleaseFramedBody_NilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("ReleaseFramedBody(nil) panicked: %v", r)
		}
	}()
	ReleaseFramedBody(nil)
}

// TestReleaseFramedBody_OversizeNotPooled confirms the 1 MiB cap on
// the pool's reuse: handing back an outlier-sized buffer (e.g. a
// max-image record) must not stash that much heap into the pool
// indefinitely. We can't directly observe the pool internals, but we
// can at least exercise the path and confirm no panic.
func TestReleaseFramedBody_OversizeNotPooled(t *testing.T) {
	// Build a >1 MiB buffer. The release path branches on cap, so a
	// length-only buffer would also work but cap is the explicit gate.
	huge := make([]byte, 0, (1<<20)+1)
	huge = huge[:cap(huge)]
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("ReleaseFramedBody(>1MiB) panicked: %v", r)
		}
	}()
	ReleaseFramedBody(huge)
}

// TestReadFramedBody_PoolReuse exercises the happy-path round-trip
// twice over the same bufio.Reader to confirm a Released buffer is
// safe to re-acquire on the next Read. The pool's behaviour is
// stochastic (sync.Pool may evict between Put and Get) but a fresh
// Get always returns a usable buffer regardless of pool state.
func TestReadFramedBody_PoolReuse(t *testing.T) {
	var buf bytes.Buffer
	for i := 0; i < 3; i++ {
		body := []byte(`{"v":1,"seq":42,"type":"entry","entry":{}}`)
		if _, err := WriteRecordRaw(&buf, body); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	br := bufio.NewReader(&buf)
	for i := 0; i < 3; i++ {
		got, _, err := ReadFramedBody(br)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		// Capture summary data BEFORE Release — the backing array may
		// be reused by a subsequent Read.
		gotLen := len(got)
		ReleaseFramedBody(got)
		if gotLen == 0 {
			t.Errorf("read %d: empty body", i)
		}
	}
}

// intToStr is a tiny helper so the tests don't depend on strconv.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
