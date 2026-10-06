package shim

import (
	"bufio"
	"io"
	"strings"
	"testing"
	"time"
)

func TestReadCappedLine(t *testing.T) {
	t.Parallel()
	// Small reader buffer so lines span several ReadSlice chunks.
	in := strings.Repeat("a", 50) + "\n" + "short\n" + strings.Repeat("b", 120) + "\n" + "tail"
	r := bufio.NewReaderSize(strings.NewReader(in), 16)
	var buf []byte
	type step struct {
		line     string
		oversize bool
		eof      bool
	}
	want := []step{
		{"", true, false}, // 50 > cap 40: skipped, stream positioned after it
		{"short", false, false},
		{"", true, false},     // 120 > cap, spans many chunks
		{"tail", false, true}, // unterminated final line comes with EOF
	}
	for i, w := range want {
		line, oversize, err := readCappedLine(r, buf, 40)
		buf = line[:0]
		if string(line) != w.line || oversize != w.oversize || (err == io.EOF) != w.eof || (err != nil && err != io.EOF) {
			t.Fatalf("step %d: line=%q oversize=%v err=%v, want %+v", i, line, oversize, err, w)
		}
	}
	if _, _, err := readCappedLine(r, buf, 40); err != io.EOF {
		t.Fatalf("after EOF: err = %v", err)
	}
}

// An over-cap stdout line must not end the read loop (#3578): the lines after
// it are still forwarded, and the CLI — which keeps writing — is drained to
// EOF instead of blocking on a full pipe. Against the Scanner-based loop this
// test times out with the CLI wedged.
func TestReadStdout_OversizeLineIsSkippedNotFatal(t *testing.T) {
	dir := t.TempDir()
	// One line well over the cap (a 12 MiB run of 'x'), then two normal lines.
	// head -c keeps the script free of a 12 MB literal.
	script := `printf '{"type":"a"}\n'; head -c 12582912 /dev/zero | tr '\0' x; printf '\n{"type":"system","subtype":"init","session_id":"sess-big"}\n{"type":"b"}\n'`
	cli, err := startCLI("sh", []string{"-c", script}, dir)
	if err != nil {
		t.Fatalf("startCLI: %v", err)
	}
	defer cli.kill()

	s := &shimServer{
		cli:    cli,
		buffer: NewRingBuffer(100, 64*1024*1024),
		state:  State{Key: "oversize:key"},
		done:   make(chan struct{}),
	}
	s.watchdog = NewWatchdog(30*time.Second, nil)
	before := stdoutOversizeTotal.Load()

	done := make(chan struct{})
	go func() { defer close(done); s.readStdout() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("readStdout did not return: the CLI is wedged on a full pipe")
	}
	if got := s.buffer.Count(); got != 3 {
		t.Errorf("buffer.Count = %d, want 3 (the two lines after the oversize one must be forwarded)", got)
	}
	if !s.sessionIDKnown.Load() {
		t.Error("session_id after the oversize line was not extracted")
	}
	if stdoutOversizeTotal.Load() != before+1 {
		t.Errorf("oversize counter moved by %d, want 1", stdoutOversizeTotal.Load()-before)
	}
}
