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
// and must not warn; genuine misses warn once per decode pass as a summary,
// not once per entry, because every restore and load-earlier page re-decodes
// the file. The reads cover both decode paths: readAllEntries and decodeFrom.
func TestDecode_MissingUUIDWarning(t *testing.T) {
	p, src, sink, _ := newPersister(t, "k")
	var inputs []clievent.EventEntry
	for i := 0; i < 40; i++ {
		inputs = append(inputs, clievent.EventEntry{UUID: "p" + rune2hex(i), Time: int64(100 + i), Type: "user"})
	}
	inputs = append(inputs,
		clievent.EventEntry{UUID: "a", Time: 1000, Type: "user", Summary: "q"},
		clievent.EventEntry{Time: 2000, Type: clievent.KindPersistGap, Detail: "dropped=3"},
		clievent.EventEntry{UUID: "b", Time: 2000, Type: "text", Summary: "after gap"},
		clievent.EventEntry{Time: 3000, Type: "text", Summary: "producer bug 1"},
		clievent.EventEntry{Time: 4000, Type: "text", Summary: "producer bug 2"},
	)
	for i := 0; i < 10; i++ {
		inputs = append(inputs, clievent.EventEntry{UUID: "t" + rune2hex(i), Time: int64(5000 + i), Type: "text"})
	}
	for _, e := range inputs {
		persistOne(t, sink, e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.Flush(ctx)

	// The page before 5005 holds a, the gap, b, both misses and t0..t4. The
	// public LoadBefore below must take the idx fast path, so one decodeFrom
	// pass serves it; a fallback would add a full-scan pass and a warning.
	const before, pageLimit = 5005, 10
	if _, ok, err := src.loadBeforeViaIdx(context.Background(), before, pageLimit); err != nil || !ok {
		t.Fatalf("loadBeforeViaIdx(%d, %d) = ok %v, err %v; fixture must exercise decodeFrom", before, pageLimit, ok, err)
	}

	logs := captureSlog(t)
	reads := []func() ([]clievent.EventEntry, error){
		func() ([]clievent.EventEntry, error) { return src.LoadLatest(context.Background(), 100) },
		func() ([]clievent.EventEntry, error) { return src.LoadLatest(context.Background(), 100) },
		func() ([]clievent.EventEntry, error) { return src.LoadBefore(context.Background(), before, pageLimit) },
	}
	for i, read := range reads {
		got, err := read()
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		var gaps, uuidless int
		for _, e := range got {
			if e.UUID == "" {
				uuidless++
				if e.Type == clievent.KindPersistGap {
					gaps++
				}
			}
		}
		if gaps != 1 || uuidless != 3 {
			t.Errorf("read %d: %d gap records and %d UUID-less entries, want 1 and 3 (UUID-less entries must be kept)", i, gaps, uuidless)
		}
	}

	var warns []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "missing UUID") {
			warns = append(warns, line)
		}
	}
	if len(warns) != len(reads) {
		t.Fatalf("missing-UUID warnings = %d, want exactly one per decode pass (%d):\n%s", len(warns), len(reads), logs)
	}
	for _, w := range warns {
		if !strings.Contains(w, "count=2") || !strings.Contains(w, "first_time=3000") {
			t.Errorf("warning must summarise the two genuine misses (count=2 first_time=3000), not the gap record:\n%s", w)
		}
	}
}
