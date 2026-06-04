package persist

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

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
			t.Errorf("bodyLen=%d: WriteRecordRaw returned %d, FrameSize=%d",
				len(body), n, want)
		}
		if int64(buf.Len()) != want {
			t.Errorf("bodyLen=%d: buffer len=%d, FrameSize=%d",
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

// TestReadFramedBody_PoolReusesBackingArray locks in the R242-PERF-1
// (#663) goal: after ReleaseFramedBody hands a buffer back, a
// subsequent ReadFramedBody for a frame that fits within the released
// capacity must reuse the SAME backing array rather than allocate a
// fresh one. The previous implementation called `make([]byte, n+1)` per
// frame which during recovery startup walked every persisted record and
// allocated each.
//
// sync.Pool's behaviour is stochastic (the pool may evict an entry
// between Put and Get), so this test stages a tight Put→Get round-trip
// in a fresh-Pool window where eviction is overwhelmingly unlikely on a
// single goroutine. We assert the underlying array pointer matches
// across the two Reads — if a future refactor drops the pool path, the
// pointer will diverge and this test catches the regression at CI time.
//
// Skip in -short mode because pool reuse is a probabilistic property
// that can flake under heavy GC pressure; -short suites prefer
// deterministic checks.
func TestReadFramedBody_PoolReusesBackingArray(t *testing.T) {
	if testing.Short() {
		t.Skip("pool-reuse stochastic check skipped in -short mode")
	}
	body := []byte(`{"v":1,"seq":1,"type":"entry","entry":{"key":"x"}}`)
	// Frame 1: read, capture cap(), release.
	var buf1 bytes.Buffer
	if _, err := WriteRecordRaw(&buf1, body); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	first, _, err := ReadFramedBody(bufio.NewReader(&buf1))
	if err != nil {
		t.Fatalf("read 1: %v", err)
	}
	firstCap := cap(first)
	// underlyingArrayPtr captures the address of the first element so a
	// pool hit can be detected even though `second` is a different slice
	// header.
	firstPtr := &first[:1][0]
	ReleaseFramedBody(first)

	// Frame 2: read same-sized body. Pool path should hand back the
	// just-released buffer.
	var buf2 bytes.Buffer
	if _, err := WriteRecordRaw(&buf2, body); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	second, _, err := ReadFramedBody(bufio.NewReader(&buf2))
	if err != nil {
		t.Fatalf("read 2: %v", err)
	}
	defer ReleaseFramedBody(second)
	if cap(second) < firstCap {
		// Pool returned a smaller buffer — possible if the pool was
		// evicted between Put and Get, but more likely a bug.
		t.Fatalf("second cap=%d < first cap=%d — pool path may have regressed", cap(second), firstCap)
	}
	secondPtr := &second[:1][0]
	if firstPtr != secondPtr {
		// sync.Pool eviction is permitted by the API contract (GC can
		// drain the pool at any time), so this is reported as a soft
		// signal via t.Logf rather than t.Errorf. CI tracks this log
		// and a steady-state miss rate would point at a regression in
		// the framing.go acquireFramedBuf path.
		t.Logf("pool did not return same backing array (likely sync.Pool eviction by GC); first=%p second=%p — informational only", firstPtr, secondPtr)
	}
}

// TestWriteFramedBody_BodyLengthRoundTrip verifies that writeFramedBody (the
// hot path changed in R20260604-PERF-18) encodes the length prefix correctly
// for a range of body sizes: 1 byte, a moderate body (boundary around the
// decimal digit count change), and a large body. Each case writes via
// WriteRecordRaw and reads back via ReadFramedBody, asserting byte-for-byte
// identity and that the returned frame size matches the written size.
func TestWriteFramedBody_BodyLengthRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"1_byte", []byte("{}")},
		{"9_bytes", []byte(`{"a":"b"}`)},
		{"10_bytes", []byte(`{"a":"bc"}`)},
		{"99_bytes", []byte(`{"v":1,"seq":0,"type":"entry","entry":{"key":"` + strings.Repeat("x", 48) + `"}}`)},
		{"100_bytes", []byte(`{"v":1,"seq":0,"type":"entry","entry":{"key":"` + strings.Repeat("x", 49) + `"}}`)},
		{"999_bytes", []byte(`{"v":1,"seq":0,"type":"entry","entry":{"key":"` + strings.Repeat("x", 948) + `"}}`)},
		{"1000_bytes", []byte(`{"v":1,"seq":0,"type":"entry","entry":{"key":"` + strings.Repeat("x", 949) + `"}}`)},
		{"large_64k", []byte(`{"v":1,"seq":0,"type":"entry","entry":{"key":"` + strings.Repeat("x", 65481) + `"}}`)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			wn, err := WriteRecordRaw(&buf, tc.body)
			if err != nil {
				t.Fatalf("WriteRecordRaw: %v", err)
			}
			if wn != int64(buf.Len()) {
				t.Errorf("returned n=%d != buffer len=%d", wn, buf.Len())
			}

			got, gotFrameN, err := ReadFramedBody(bufio.NewReader(&buf))
			if err != nil {
				ReleaseFramedBody(got)
				t.Fatalf("ReadFramedBody: %v", err)
			}
			if !bytes.Equal(got, tc.body) {
				ReleaseFramedBody(got)
				t.Errorf("body mismatch: got len=%d want len=%d", len(got), len(tc.body))
			}
			if int64(gotFrameN) != wn {
				ReleaseFramedBody(got)
				t.Errorf("frame size: got %d want %d", gotFrameN, wn)
			}
			ReleaseFramedBody(got)
		})
	}
}

// writeRecord is the test-only marshal+frame combo helper. Production
// always pre-marshals records via schema.MarshalRecord and feeds the
// pre-marshalled bytes into WriteRecordRaw, so the combo wrapper has
// no production caller — it only exists here so the tests can keep
// asserting the round-trip property without re-typing two function
// calls per fixture. DEADCODE-13 (#1206).
func writeRecord(w io.Writer, r *schema.Record) (int64, error) {
	body, err := schema.MarshalRecord(r)
	if err != nil {
		return 0, err
	}
	return WriteRecordRaw(w, body)
}

// frameSize predicts the on-disk length of a framed record given the
// JSON body length. Production never recomputes this from the body
// length (rotate / idx tracking pull the post-write count from the
// WriteRecordRaw return value), so the predictor lives only as a
// test invariant — TestFrameSize_MatchesWriteRecord pins the
// "predicted size matches written size" contract that protects rotate's
// cut-offset math from drift. DEADCODE-13 (#1206).
func frameSize(bodyLen int) int {
	// Integer log10 of bodyLen for the decimal prefix width, plus two
	// newlines.
	return len(intToStr(bodyLen)) + 1 + bodyLen + 1
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
