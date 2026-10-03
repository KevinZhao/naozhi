package naozhilog

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// captureSlog swaps slog.Default for a text handler writing to the returned
// buffer. Tests using it must not call t.Parallel.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestDecode_MissingUUIDWarning: persist's gap record is UUID-less by design
// and must not warn; genuine misses warn once per read as a summary, not once
// per entry, because every restore and load-earlier page re-decodes the file.
func TestDecode_MissingUUIDWarning(t *testing.T) {
	p, src, sink, _ := newPersister(t, "k")
	inputs := []clievent.EventEntry{
		{UUID: "a", Time: 1000, Type: "user", Summary: "q"},
		{Time: 2000, Type: clievent.KindPersistGap, Detail: "dropped=3"},
		{UUID: "b", Time: 2000, Type: "text", Summary: "after gap"},
		{Time: 3000, Type: "text", Summary: "producer bug 1"},
		{Time: 4000, Type: "text", Summary: "producer bug 2"},
	}
	for _, e := range inputs {
		persistOne(t, sink, e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.Flush(ctx)

	logs := captureSlog(t)
	reads := []func() ([]clievent.EventEntry, error){
		func() ([]clievent.EventEntry, error) { return src.LoadLatest(context.Background(), 100) },
		func() ([]clievent.EventEntry, error) { return src.LoadLatest(context.Background(), 100) },
		func() ([]clievent.EventEntry, error) { return src.LoadBefore(context.Background(), 0, 100) },
	}
	for i, read := range reads {
		got, err := read()
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if len(got) != len(inputs) {
			t.Fatalf("read %d: got %d entries, want %d (UUID-less entries must be kept)", i, len(got), len(inputs))
		}
		if got[1].Type != clievent.KindPersistGap || got[1].UUID != "" {
			t.Errorf("read %d: entry[1] = %+v, want the UUID-less gap record", i, got[1])
		}
	}

	var warns []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "missing UUID") {
			warns = append(warns, line)
		}
	}
	if len(warns) != len(reads) {
		t.Fatalf("missing-UUID warnings = %d, want exactly one per read (%d):\n%s", len(warns), len(reads), logs)
	}
	for _, w := range warns {
		if !strings.Contains(w, "count=2") || !strings.Contains(w, "first_time=3000") {
			t.Errorf("warning must summarise the two genuine misses (count=2 first_time=3000), not the gap record:\n%s", w)
		}
	}
}
