//go:build unix

package datadir

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// openLog creates a log file holding n 20-byte lines and returns it opened with
// flag, the way an init system would hand it to naozhi.
func openLog(t *testing.T, flag, n int) (*os.File, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdout.log")
	var body bytes.Buffer
	for i := range n {
		fmt.Fprintf(&body, "line %06d xxxxxxx\n", i) // 20 bytes
	}
	if err := os.WriteFile(path, body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f, body.Bytes()
}

func readAll(t *testing.T, f *os.File) []byte {
	t.Helper()
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// splitMarker returns the marker record and what follows it.
func splitMarker(t *testing.T, content []byte) (map[string]any, []byte) {
	t.Helper()
	line, rest, ok := bytes.Cut(content, []byte("\n"))
	if !ok {
		t.Fatalf("no marker line in %q", content)
	}
	var rec map[string]any
	if err := json.Unmarshal(line, &rec); err != nil {
		t.Fatalf("marker is not a JSON record: %q: %v", line, err)
	}
	return rec, rest
}

// TestCapStdioKeepsTheNewestWholeLinesInPlace is the launchd case (O_RDWR |
// O_APPEND): the file shrinks to a marker plus its newest whole lines, stays
// the same inode, and the next write lands at the new end with no hole. A
// trailing fragment (a write still in progress) is dropped, not kept.
func TestCapStdioKeepsTheNewestWholeLinesInPlace(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		keepTail  int64
		fragment  string
		wantLines int
	}{
		{"window starts on a line boundary", 100, "", 5},
		{"window starts mid-line", 110, "", 5},
		{"window smaller than one line", 10, "", 0},
		{"file ends in a partial line", 100, "partial", 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, lines := openLog(t, os.O_RDWR|os.O_APPEND, 200)
			if _, err := f.WriteString(tc.fragment); err != nil {
				t.Fatal(err)
			}
			orig := append(bytes.Clone(lines), tc.fragment...)
			before, err := os.Stat(f.Name())
			if err != nil {
				t.Fatal(err)
			}

			res, err := CapStdio(f, "stdout", 1000, tc.keepTail)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Truncated || res.Skip != "" || res.BytesBefore != int64(len(orig)) {
				t.Fatalf("result %+v, want truncated with bytes_before=%d", res, len(orig))
			}
			rec, tail := splitMarker(t, readAll(t, f))
			want := lines[len(lines)-20*tc.wantLines:]
			if !bytes.Equal(tail, want) {
				t.Errorf("kept tail %q, want the last %d lines %q", tail, tc.wantLines, want)
			}
			if res.KeptBytes != int64(len(want)) || rec["kept_bytes"] != float64(len(want)) {
				t.Errorf("kept_bytes result=%d marker=%v, want %d", res.KeptBytes, rec["kept_bytes"], len(want))
			}
			if rec["stream"] != "stdout" || rec["bytes_before"] != float64(len(orig)) {
				t.Errorf("marker %v", rec)
			}

			after, err := os.Stat(f.Name())
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) {
				t.Error("the file was replaced; the init system's fd would now point at a different inode")
			}
			if _, err := f.WriteString("after\n"); err != nil {
				t.Fatal(err)
			}
			got := readAll(t, f)
			if int64(len(got)) != after.Size()+6 || !bytes.HasSuffix(got, []byte("after\n")) {
				t.Errorf("write after the cap: size %d, want %d (a hole means the fd was not appending)",
					len(got), after.Size()+6)
			}
		})
	}
}

// TestCapStdioLeavesFilesItMustNotTouch covers every skip: disabled, under the
// cap, and an over-cap file opened without O_APPEND, where truncating would
// leave the next write at the old offset behind a hole.
func TestCapStdioLeavesFilesItMustNotTouch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		flag    int
		maxSize int64
		want    StdioSkip
	}{
		{"disabled", os.O_RDWR | os.O_APPEND, 0, StdioSkipDisabled},
		{"negative disables", os.O_RDWR | os.O_APPEND, -1, StdioSkipDisabled},
		{"under the cap", os.O_RDWR | os.O_APPEND, 4000, StdioSkipUnderCap},
		{"not append", os.O_RDWR, 1000, StdioSkipNotAppend},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, orig := openLog(t, tc.flag, 200)
			res, err := CapStdio(f, "stdout", tc.maxSize, 100)
			if err != nil {
				t.Fatal(err)
			}
			if res.Skip != tc.want || res.Truncated {
				t.Errorf("result %+v, want skip %q", res, tc.want)
			}
			if !bytes.Equal(readAll(t, f), orig) {
				t.Error("the file was modified")
			}
		})
	}
}

// TestCapStdioSkipsAPipe: journald hands over a socket and a terminal is a
// tty; neither is a file to cap.
func TestCapStdioSkipsAPipe(t *testing.T) {
	t.Parallel()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	res, err := CapStdio(w, "stdout", 1, 1)
	if err != nil || res.Skip != StdioSkipNotRegular {
		t.Errorf("CapStdio(pipe) = %+v, %v; want skip %q", res, err, StdioSkipNotRegular)
	}
}

// TestCapStdioWriteOnlyAppendFd is systemd's StandardOutput=append:, which
// hands over a write-only fd. The cap must still fire; the tail comes from a
// reopen of the same inode where the platform allows it (Linux), and is
// empty otherwise.
func TestCapStdioWriteOnlyAppendFd(t *testing.T) {
	t.Parallel()
	f, orig := openLog(t, os.O_WRONLY|os.O_APPEND, 200)
	res, err := CapStdio(f, "stderr", 1000, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatalf("result %+v, want truncated", res)
	}
	_, tail := splitMarker(t, readAll(t, f))
	want := orig[len(orig)-100:]
	if runtime.GOOS != "linux" {
		want = nil
	}
	if !bytes.Equal(tail, want) {
		t.Errorf("tail %q, want %q", tail, want)
	}
}

// captureSlog swaps the default logger for the test. Callers must not be
// parallel: the default logger is process-global.
func captureSlog(t *testing.T) *recorder {
	t.Helper()
	rec := &recorder{}
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return rec
}

type recorder struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec.Clone())
	return nil
}
func (r *recorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recorder) WithGroup(string) slog.Handler      { return r }

func (r *recorder) find(level slog.Level, msgPrefix string) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]any
	for _, rec := range r.recs {
		if rec.Level != level || !strings.HasPrefix(rec.Message, msgPrefix) {
			continue
		}
		attrs := map[string]any{}
		rec.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.Any(); return true })
		out = append(out, attrs)
	}
	return out
}

// TestCapStdioLogsOneInfo: the acceptance criterion is an INFO line naming the
// stream and what was dropped, so a shrunken log is never a mystery.
func TestCapStdioLogsOneInfo(t *testing.T) {
	logs := captureSlog(t)
	f, orig := openLog(t, os.O_RDWR|os.O_APPEND, 200)
	if _, err := CapStdio(f, "stdout", 1000, 100); err != nil {
		t.Fatal(err)
	}
	got := logs.find(slog.LevelInfo, "stdio log truncated")
	if len(got) != 1 {
		t.Fatalf("got %d INFO records, want 1: %v", len(got), got)
	}
	want := map[string]any{"stream": "stdout", "bytes_before": int64(len(orig)), "kept_bytes": int64(100), "max": int64(1000)}
	for k, v := range want {
		if got[0][k] != v {
			t.Errorf("attr %s = %v (%T), want %v", k, got[0][k], got[0][k], v)
		}
	}
}

// TestStdioTaskWarnsOnceForANonAppendFd: an fd the cap must leave alone is an
// operator problem worth one WARN, not one every hour.
func TestStdioTaskWarnsOnceForANonAppendFd(t *testing.T) {
	logs := captureSlog(t)
	f, orig := openLog(t, os.O_RDWR, 200)
	task := StdioTask(f, "stdout", 1000)
	task()
	task()
	if n := len(logs.find(slog.LevelWarn, "stdio log is over its cap")); n != 1 {
		t.Errorf("got %d WARNs over two sweeps, want 1", n)
	}
	if !bytes.Equal(readAll(t, f), orig) {
		t.Error("a non-append file was modified")
	}
}

// TestStdioTaskKeepsAnEighthCappedAt4MB pins the tail budget StdioTask passes
// to CapStdio.
func TestStdioTaskKeepsAnEighthCappedAt4MB(t *testing.T) {
	t.Parallel()
	f, orig := openLog(t, os.O_RDWR|os.O_APPEND, 200)
	StdioTask(f, "stdout", 800)() // keeps 100 bytes: exactly five lines
	_, tail := splitMarker(t, readAll(t, f))
	if want := orig[len(orig)-100:]; !bytes.Equal(tail, want) {
		t.Errorf("tail %q, want %q", tail, want)
	}

	// A large cap keeps 4MB, not an eighth: 40MB of sparse hole, then 5MB of
	// lines, capped at 40MB. The 4MB window starts mid-line, so the kept tail
	// is the whole lines inside it.
	path := filepath.Join(t.TempDir(), "big.log")
	big, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer big.Close()
	if err := big.Truncate(40 << 20); err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(big)
	for i := range (5 << 20) / 20 {
		fmt.Fprintf(w, "line %06d xxxxxxx\n", i%1000000)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	StdioTask(big, "stdout", 40<<20)()
	_, tail = splitMarker(t, readAll(t, big))
	if want := (stdioMaxTail / 20) * 20; len(tail) != want {
		t.Errorf("kept %d bytes, want %d", len(tail), want)
	}
}
