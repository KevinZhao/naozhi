package subagent

import (
	"os"
	"regexp"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestTranscriptReader_EntryUUIDStableAcrossReaders pins the identity the
// dashboard's agent drill-in dedups by (#3669 C): an HTTP page (a fresh
// reader filtering by time) and the WS tailer (a long-lived reader tailing in
// pieces) must give each entry the same UUID, including entries with Time 0,
// which no time watermark can order. Identical content on two records must
// still yield two identities.
func TestTranscriptReader_EntryUUIDStableAcrossReaders(t *testing.T) {
	t.Parallel()
	timed := `{"type":"user","uuid":"u1","message":{"role":"user","content":"go"},"timestamp":"2026-05-10T10:00:00Z"}`
	// No timestamp: maps to Time 0. Two blocks on one line, then a record
	// with the same content but its own uuid.
	untimed1 := `{"type":"assistant","uuid":"u2","message":{"role":"assistant","content":[{"type":"text","text":"same"},{"type":"tool_use","name":"Read","input":{"file_path":"/a"}}]}}`
	untimed2 := `{"type":"assistant","uuid":"u3","message":{"role":"assistant","content":[{"type":"text","text":"same"}]}}`
	later := `{"type":"assistant","uuid":"u4","message":{"role":"assistant","content":[{"type":"text","text":"later"}]},"timestamp":"2026-05-10T10:00:05Z"}`

	path := tmpFile(t, timed+"\n"+untimed1+"\n")
	tailer := NewTranscriptReader(path)
	defer tailer.Close()
	first, err := tailer.Tail()
	if err != nil {
		t.Fatalf("Tail 1: %v", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString(untimed2 + "\n" + later + "\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()
	rest, err := tailer.Tail()
	if err != nil {
		t.Fatalf("Tail 2: %v", err)
	}
	tailed := append(first, rest...)

	page := NewTranscriptReader(path)
	defer page.Close()
	full, err := page.Read(0, 100)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(full) != 5 || len(tailed) != 5 {
		t.Fatalf("entries: page %d, tailer %d, want 5 each", len(full), len(tailed))
	}
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	seen := map[string]int{}
	for i := range full {
		if !hex32.MatchString(full[i].UUID) {
			t.Fatalf("entry %d UUID %q is not 32 lowercase hex", i, full[i].UUID)
		}
		if full[i].UUID != tailed[i].UUID {
			t.Errorf("entry %d: page UUID %s != tailer UUID %s", i, full[i].UUID, tailed[i].UUID)
		}
		if j, dup := seen[full[i].UUID]; dup {
			t.Errorf("entries %d and %d share UUID %s", j, i, full[i].UUID)
		}
		seen[full[i].UUID] = i
	}
	if full[1].Time != 0 || full[3].Time != 0 {
		t.Fatalf("fixture drift: entries 1 and 3 must be untimed, got %d, %d", full[1].Time, full[3].Time)
	}

	// A later poll page filters by time; the untimed entries it re-admits
	// keep the identity the first page gave them.
	after := NewTranscriptReader(path)
	defer after.Close()
	polled, err := after.Read(clievent.SinceInclusive(full[4].Time), 100)
	if err != nil {
		t.Fatalf("Read after: %v", err)
	}
	want := []string{full[1].UUID, full[2].UUID, full[3].UUID, full[4].UUID}
	if len(polled) != len(want) {
		t.Fatalf("poll page: %d entries, want %d", len(polled), len(want))
	}
	for i, e := range polled {
		if e.UUID != want[i] {
			t.Errorf("poll entry %d UUID %s, want %s", i, e.UUID, want[i])
		}
	}
}
