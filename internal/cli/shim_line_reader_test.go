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
