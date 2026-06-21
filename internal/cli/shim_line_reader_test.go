package cli

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

// TestShimLineReader_BoundsNonStdoutFlood pins R237-GO-6 (#633): the
// init-handshake LineReader has a bounded skip counter so a buggy /
// hostile shim that streams non-stdout/non-cli_exited frames forever
// cannot wedge proto.Init. The LineReader interface has no ctx
// parameter (changing it would be breaking across both Claude and ACP
// backends) so the structural limit lives inside ReadLine itself.
func TestShimLineReader_BoundsNonStdoutFlood(t *testing.T) {
	// Build a buffer with shimLineReaderMaxSkips+1 ping frames followed by
	// a stdout frame. The stdout frame is unreachable because the skip
	// counter trips first.
	var buf bytes.Buffer
	for i := 0; i < shimLineReaderMaxSkips+1; i++ {
		buf.WriteString(`{"type":"pong"}` + "\n")
	}
	buf.WriteString(`{"type":"stdout","line":"unreachable"}` + "\n")

	r := &shimLineReader{
		proc: &Process{shimR: bufio.NewReader(&buf)},
	}

	data, eof, err := r.ReadLine()
	if err == nil {
		t.Fatalf("expected error after %d non-stdout frames, got nil", shimLineReaderMaxSkips+1)
	}
	if !eof {
		t.Errorf("expected eof=true on flood-trip, got eof=false")
	}
	if data != nil {
		t.Errorf("expected nil data on flood-trip, got %q", data)
	}
	if !strings.Contains(err.Error(), "shim sent") {
		t.Errorf("error message should mention frame count + cause, got %q", err.Error())
	}
}

// TestShimLineReader_BoundsUnparseableFlood mirrors the non-stdout flood
// test for raw-junk frames that fail json.Unmarshal — these also drive
// the skip counter and trip the same bound.
func TestShimLineReader_BoundsUnparseableFlood(t *testing.T) {
	var buf bytes.Buffer
	for i := 0; i < shimLineReaderMaxSkips+1; i++ {
		buf.WriteString("not-json-at-all\n")
	}

	r := &shimLineReader{
		proc: &Process{shimR: bufio.NewReader(&buf)},
	}

	_, eof, err := r.ReadLine()
	if err == nil {
		t.Fatalf("expected error after %d unparseable frames, got nil", shimLineReaderMaxSkips+1)
	}
	if !eof {
		t.Errorf("expected eof=true on flood-trip, got eof=false")
	}
	if !strings.Contains(err.Error(), "unparseable") {
		t.Errorf("error message should mention unparseable cause, got %q", err.Error())
	}
}

// TestShimLineReader_HappyPathReturnsStdout verifies the bounded counter
// does not impact the normal path: a single stdout frame returns its
// line without tripping the limit.
func TestShimLineReader_HappyPathReturnsStdout(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(`{"type":"stdout","line":"hello"}` + "\n")

	r := &shimLineReader{
		proc: &Process{shimR: bufio.NewReader(&buf)},
	}

	data, eof, err := r.ReadLine()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eof {
		t.Errorf("unexpected eof=true on stdout frame")
	}
	if string(data) != "hello" {
		t.Errorf("data = %q, want %q", string(data), "hello")
	}
}

// TestShimLineReader_HappyPathSkipsHandfulOfPings verifies a small number
// of ping/stderr frames before a stdout still resolves to the stdout line.
// This is the realistic transient-warmup case the bounded counter MUST
// permit; only an unbounded flood should trip the limit.
func TestShimLineReader_HappyPathSkipsHandfulOfPings(t *testing.T) {
	var buf bytes.Buffer
	for i := 0; i < 8; i++ {
		buf.WriteString(`{"type":"pong"}` + "\n")
	}
	buf.WriteString(`{"type":"stdout","line":"after-warmup"}` + "\n")

	r := &shimLineReader{
		proc: &Process{shimR: bufio.NewReader(&buf)},
	}

	data, eof, err := r.ReadLine()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eof {
		t.Errorf("unexpected eof=true after warmup")
	}
	if string(data) != "after-warmup" {
		t.Errorf("data = %q, want %q", string(data), "after-warmup")
	}
}

// TestShimLineReader_BoundsOversizeLine pins #2183: a single init-handshake
// frame larger than maxScannerBufBytes with no newline must be rejected with
// a cap error (eof=true, data==nil) rather than growing bufio's buffer
// without bound -> OOM. ReadSlice returns ErrBufferFull mid-frame, so the
// accumulate loop MUST treat ErrBufferFull as non-terminal until the running
// length trips the cap.
func TestShimLineReader_BoundsOversizeLine(t *testing.T) {
	// One giant frame, no trailing newline, well past the cap. Sized at
	// twice the cap so the running-length guard trips on a mid-stream
	// ErrBufferFull chunk (the OOM scenario) rather than at EOF.
	giant := bytes.Repeat([]byte("a"), 2*maxScannerBufBytes)

	r := &shimLineReader{
		proc: &Process{shimR: bufio.NewReader(bytes.NewReader(giant))},
	}

	data, eof, err := r.ReadLine()
	if err == nil {
		t.Fatalf("expected error on oversize init line, got nil")
	}
	if !eof {
		t.Errorf("expected eof=true on cap, got false")
	}
	if data != nil {
		t.Errorf("expected nil data on cap, got %d bytes", len(data))
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should mention the byte cap, got %q", err.Error())
	}
}

// TestShimLineReader_HappyPathSpansBufferBoundary verifies the
// ReadSlice-accumulate loop reassembles a stdout frame whose JSON envelope
// is larger than bufio's internal buffer (forcing ErrBufferFull mid-frame)
// but still under the cap. This guards the regression where ReadSlice —
// unlike the old ReadBytes — surfaces ErrBufferFull that, if treated as
// terminal, would truncate or drop legitimate large frames.
func TestShimLineReader_HappyPathSpansBufferBoundary(t *testing.T) {
	// A stdout line payload bigger than the bufio.Reader buffer below.
	payload := strings.Repeat("x", 16*1024)
	frame := `{"type":"stdout","line":"` + payload + `"}` + "\n"

	r := &shimLineReader{
		// Small reader buffer forces ReadSlice to return ErrBufferFull
		// repeatedly before the newline is reached.
		proc: &Process{shimR: bufio.NewReaderSize(strings.NewReader(frame), 1024)},
	}

	data, eof, err := r.ReadLine()
	if err != nil {
		t.Fatalf("unexpected error reassembling boundary-spanning frame: %v", err)
	}
	if eof {
		t.Errorf("unexpected eof=true on valid large frame")
	}
	if string(data) != payload {
		t.Errorf("data len = %d, want %d (line truncated across buffer boundary)", len(data), len(payload))
	}
}

// TestShimLineReader_CLIExitedReturnsErrorImmediately verifies that a
// cli_exited frame short-circuits the loop with an error, regardless of
// the skip counter state. This mirrors the existing contract — a CLI
// crash mid-handshake must surface to Spawn's error path so the shim is
// torn down.
func TestShimLineReader_CLIExitedReturnsErrorImmediately(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(`{"type":"cli_exited"}` + "\n")

	r := &shimLineReader{
		proc: &Process{shimR: bufio.NewReader(&buf)},
	}

	data, eof, err := r.ReadLine()
	if err == nil {
		t.Fatalf("expected cli_exited error, got nil")
	}
	if !eof {
		t.Errorf("expected eof=true on cli_exited, got false")
	}
	if data != nil {
		t.Errorf("expected nil data, got %q", data)
	}
	if !strings.Contains(err.Error(), "cli exited") {
		t.Errorf("error should mention cli exited, got %q", err.Error())
	}
}
