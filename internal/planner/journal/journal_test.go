package journal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func openUTC(t *testing.T) *Journal {
	t.Helper()
	j, err := Open(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	j.loc = time.UTC
	t.Cleanup(func() { j.Close() })
	return j
}

func units(es []Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Unit
	}
	return out
}

func TestAppendRead(t *testing.T) {
	j := openUTC(t)
	oct := time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC)
	nov := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	mustAppend := func(e Entry) {
		t.Helper()
		if err := j.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	mustAppend(Entry{TS: oct, WS: "ws_aaaaaaaa", Kind: KindHealth, Unit: "a", Data: json.RawMessage(`{"to":"idle"}`)})
	mustAppend(Entry{TS: nov, Kind: KindNotify, Unit: "b"})

	// Lines this build must not surface: another version, an unknown kind,
	// garbage, and a torn tail left by a crash (no trailing newline).
	novFile := filepath.Join(j.dir, "2026-11.jsonl")
	f, err := os.OpenFile(novFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, `{"v":2,"ts":"2026-11-01T02:00:00Z","kind":"health","unit":"v2"}`+"\n")
	fmt.Fprintf(f, `{"v":1,"ts":"2026-11-01T02:00:00Z","kind":"promise","unit":"unknown"}`+"\n")
	fmt.Fprintf(f, "not json\n")
	fmt.Fprintf(f, `{"v":1,"ts":"2026-11-01T02:00:00Z","kind":"hea`)
	f.Close()

	// Reopening the torn file must put the next entry on a line of its own.
	j.Close()
	mustAppend(Entry{TS: nov.Add(time.Hour), Kind: KindBrief, Unit: "c"})

	if err := j.Append(Entry{Kind: "promise"}); err == nil {
		t.Error("Append accepted an unknown kind")
	}

	got, err := j.Read(oct.Add(-time.Hour), nov.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "c"}; !slices.Equal(units(got), want) {
		t.Fatalf("units = %v, want %v", units(got), want)
	}
	if got[0].V != Version || got[0].WS != "ws_aaaaaaaa" || string(got[0].Data) != `{"to":"idle"}` {
		t.Errorf("first entry = %+v", got[0])
	}
	if string(got[1].Data) != "{}" {
		t.Errorf("empty Data written as %s, want {}", got[1].Data)
	}

	// The window is half-open and spans month files.
	got, err = j.Read(nov, nov.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"b"}; !slices.Equal(units(got), want) {
		t.Errorf("windowed units = %v, want %v", units(got), want)
	}
}

func TestPrune(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		month string
		kept  bool
	}{
		{"2026-11", true},
		{"2026-05", true},  // ended 2026-06-01, 155 days ago
		{"2026-04", false}, // ended 2026-05-01, 186 days ago
		{"2025-12", false},
	}
	j := openUTC(t)
	for _, c := range cases {
		if err := os.WriteFile(filepath.Join(j.dir, c.month+".jsonl"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	other := filepath.Join(j.dir, "notes.txt")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := j.Prune(now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("removed %d files, want 2", n)
	}
	for _, c := range cases {
		_, err := os.Stat(filepath.Join(j.dir, c.month+".jsonl"))
		if kept := err == nil; kept != c.kept {
			t.Errorf("%s kept = %v, want %v", c.month, kept, c.kept)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("Prune touched a non-journal file: %v", err)
	}
}

func TestConcurrentAppend(t *testing.T) {
	j := openUTC(t)
	ts := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	const writers, each = 8, 50
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range each {
				if err := j.Append(Entry{TS: ts, Kind: KindHeartbeat, Unit: fmt.Sprintf("%d-%d", w, i)}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	got, err := j.Read(ts, ts.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != writers*each {
		t.Fatalf("read %d entries, want %d", len(got), writers*each)
	}
}
