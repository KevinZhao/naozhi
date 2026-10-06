package workflow_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/workflow"
)

// pickLines returns the probe's lines with the given 1-based numbers.
func pickLines(all []string, nums ...int) []string {
	out := make([]string, 0, len(nums))
	for _, n := range nums {
		out = append(out, all[n-1])
	}
	return out
}

func span(from, to int, skip ...int) []int {
	var out []int
	for n := from; n <= to; n++ {
		if !contains(skip, n) {
			out = append(out, n)
		}
	}
	return out
}

func contains(xs []int, x int) bool {
	for _, y := range xs {
		if x == y {
			return true
		}
	}
	return false
}

func seed(tb testing.TB, lines []string, wrapped bool, known ...string) (*workflow.Tracker, int64) {
	tb.Helper()
	tr := workflow.New(nil)
	dec := &countingDecoder{}
	tr.SeedFromReplay(workflow.Replay{Lines: lines, Wrapped: wrapped}, dec, known)
	return tr, dec.n.Load()
}

// TestSeed_ProbeThroughLine16 restarts after the notification: the picks are
// applied oldest first, so the terminal frames do not shut out the snapshot.
func TestSeed_ProbeThroughLine16(t *testing.T) {
	t.Parallel()
	all := probeLines(t)
	tr, decodes := seed(t, pickLines(all, span(1, 16)...), false)
	if decodes != 6 { // 4, 5, 12, 13, 15, 16; the older snapshots 6, 7, 8, 11 are skipped
		t.Errorf("%d lines decoded, want 6", decodes)
	}
	got := only(t, tr)
	live := workflow.New(nil)
	feed(t, live, time.UnixMilli(1791170018000), pickLines(all, span(1, 16)...)...)
	want := *only(t, live)
	// What only a live read can know: arrival times, every snapshot seen.
	want.StartedAt, want.Src.StartedAt = 1791170018846, workflow.StartedFromSnapshot
	want.LastObservedAt, want.Source = 0, workflow.SourceReplay
	want.TrackerVersion, want.SnapshotSeq = got.TrackerVersion, 1
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("seeded:\n got %+v\nwant %+v", *got, want)
	}
	if tr.Load().SeedWrapped {
		t.Error("SeedWrapped set for an unwrapped replay")
	}
}

// TestSeed_NewestOfEachClassWins: the description-only line 12 is newer
// than snapshot 11 and older than snapshot 13; the header follows the
// newest, and a run ending on snapshot 11 stops its running agent.
func TestSeed_NewestOfEachClassWins(t *testing.T) {
	t.Parallel()
	all := probeLines(t)
	tr, _ := seed(t, pickLines(all, span(1, 15, 14)...), false)
	if w := only(t, tr); w.DurationMS != 12764 || w.Status != workflow.StatusCompleted {
		t.Errorf("through line 15: duration %d (line 13 has 12764, line 12 12758), status %s", w.DurationMS, w.Status)
	}
	tr, _ = seed(t, pickLines(all, span(1, 15, 13, 14)...), false)
	w := only(t, tr)
	if w.DurationMS != 12758 || len(w.Agents) != 3 {
		t.Fatalf("without line 13: duration %d (line 12 has 12758, line 11 7954), %d rows", w.DurationMS, len(w.Agents))
	}
	if c := w.Agents[2]; c.Label != "C" || c.State != workflow.AgentStopped || w.Counts != (workflow.Counts{Total: 3, Done: 2, Stopped: 1}) {
		t.Errorf("C still running at the end: %+v, counts %+v", c, w.Counts)
	}
}

// TestSeed_WithoutLaunchFrames: the ring lost task_started and the launch
// receipt; the snapshot (rule 3) builds the entry and the terminal frames
// follow it (rule 2b).
func TestSeed_WithoutLaunchFrames(t *testing.T) {
	t.Parallel()
	all := probeLines(t)
	tr, _ := seed(t, pickLines(all, span(1, 16, 4, 5)...), true)
	w := only(t, tr)
	if w.Status != workflow.StatusCompleted || len(w.Agents) != 3 || w.Counts.Done != 3 || w.Tokens != 53217 {
		t.Fatalf("without lines 4/5: %+v", *w)
	}
	if w.Name != "tiny probe" || w.Src.Name != workflow.NameFromSummary || w.RunID != "" ||
		w.SessionID != probeSession || w.Src.SessionID != workflow.SessionFromProgress || w.LastObservedAt != 0 {
		t.Errorf("fallback header: %+v src %+v", *w, w.Src)
	}
	if !tr.Load().SeedWrapped {
		t.Error("SeedWrapped not set")
	}
}

// TestSeed_OnlyTerminalFrames: with nothing but lines 15/16 left, only an
// id the session already knows builds an entry.
func TestSeed_OnlyTerminalFrames(t *testing.T) {
	t.Parallel()
	all := probeLines(t)
	tail := pickLines(all, 14, 15, 16, 17, 18, 19, 20)
	tr, _ := seed(t, tail, true)
	if n := len(tr.Load().Workflows); n != 0 {
		t.Fatalf("unknown task built %d entries", n)
	}
	tr, _ = seed(t, tail, true, probeTask)
	w := only(t, tr)
	if w.Status != workflow.StatusCompleted || w.Agents != nil || w.Degraded != workflow.DegradedNoSnapshot ||
		w.EndedAt != 1791170031523 || w.Tokens != 53217 || w.NotifySummary == "" {
		t.Fatalf("header-only entry: %+v", *w)
	}
}

// replayLine builds small task_* lines like the ones that fill a wrapped ring.
func smallProgress(task string, n int) string {
	return fmt.Sprintf(`{"type":"system","subtype":"task_progress","task_id":%q,"tool_use_id":"toolu_x","description":"Impl: impl:%d","usage":{"total_tokens":%d,"tool_uses":1,"duration_ms":%d},"last_tool_name":"impl","summary":"batch","uuid":"u","session_id":%q}`, task, n, n, n, probeSession)
}

// TestSeed_DecodesPerTaskNotPerLine: a wrapped 10k-line ring of small
// progress frames costs a few decodes per task, running workflows included.
func TestSeed_DecodesPerTaskNotPerLine(t *testing.T) {
	t.Parallel()
	var lines []string
	tasks := []string{"wone00001", "wtwo00002", "wthr00003"}
	for i := 0; i < 10000; i++ {
		task := tasks[i%3]
		switch {
		case i%1000 == 0:
			lines = append(lines, string(bigSnapshot(20, bigOpts{task: task, running: 2, tick: i})))
		case i == 9990:
			lines = append(lines, fmt.Sprintf(`{"type":"system","subtype":"task_updated","task_id":%q,"patch":{"status":"completed","end_time":5}}`, task))
		default:
			lines = append(lines, smallProgress(task, i))
		}
		lines = append(lines, `{"type":"assistant","message":{"role":"assistant","content":[]}}`)
	}
	tr, decodes := seed(t, lines, true)
	// Per task: newest snapshot, newest header-only line, and for the one
	// that ended its task_updated.
	if decodes != 7 {
		t.Errorf("%d decodes for %d lines, want 7", decodes, len(lines))
	}
	for _, w := range tr.Load().Workflows {
		if len(w.Agents) != 20 || w.LastObservedAt != 0 || w.Source != workflow.SourceReplay {
			t.Errorf("%s: %d rows, observed %d, source %q", w.TaskID, len(w.Agents), w.LastObservedAt, w.Source)
		}
	}
	if n := len(tr.Load().Workflows); n != 3 {
		t.Fatalf("%d entries, want 3", n)
	}
}

// TestSeed_FallbackDecodeForOddTaskID: an id the prefix scan does not
// trust is learned by decoding, and still counts against its class.
func TestSeed_FallbackDecodeForOddTaskID(t *testing.T) {
	t.Parallel()
	lines := []string{string(bigSnapshot(3, bigOpts{task: "W-Odd"})), string(bigSnapshot(4, bigOpts{task: "W-Odd"}))}
	tr, decodes := seed(t, lines, false)
	if decodes != 2 {
		t.Errorf("%d decodes, want 2 (both lines need one to learn the id)", decodes)
	}
	if w := only(t, tr); w.TaskID != "W-Odd" || len(w.Agents) != 4 {
		t.Fatalf("odd id entry: %s with %d rows, want the newer snapshot's 4", w.TaskID, len(w.Agents))
	}
	long := strings.Repeat("w", 33) // past the 32-byte bound, all in the class
	lines = []string{string(bigSnapshot(3, bigOpts{task: long})), string(bigSnapshot(4, bigOpts{task: long}))}
	if _, decodes := seed(t, lines, false); decodes != 2 {
		t.Errorf("33-byte id: %d decodes, want 2", decodes)
	}
}

// TestSeed_FailedNewestSnapshotFallsBack: live keeps the rows of the last
// snapshot that decoded when a newer one fails the identity check; so does
// the seed, trying at most three older snapshot lines.
func TestSeed_FailedNewestSnapshotFallsBack(t *testing.T) {
	t.Parallel()
	good := string(bigSnapshot(3, bigOpts{}))
	bad := strings.Replace(string(bigSnapshot(4, bigOpts{})), `"type":"workflow_agent","index":2,`, `"type":"workflow_agent","index":"x",`, 1)
	if bad == string(bigSnapshot(4, bigOpts{})) {
		t.Fatal("fixture: no agent index replaced")
	}
	live := workflow.New(nil)
	feed(t, live, time.UnixMilli(1791170018000), good, bad)
	want := only(t, live)
	tr, decodes := seed(t, []string{good, bad}, false)
	got := only(t, tr)
	if len(want.Agents) != 3 || want.Degraded != workflow.DegradedDecodeError {
		t.Fatalf("live: %d rows, %q", len(want.Agents), want.Degraded)
	}
	if len(got.Agents) != 3 || got.Degraded != want.Degraded || got.Counts != want.Counts || decodes != 2 {
		t.Fatalf("seeded: %d rows, %q, counts %+v, %d decodes", len(got.Agents), got.Degraded, got.Counts, decodes)
	}
	tr, decodes = seed(t, []string{good, bad, bad, bad, bad}, false)
	if w := only(t, tr); len(w.Agents) != 0 || w.Degraded != workflow.DegradedDecodeError || decodes != 4 {
		t.Fatalf("fallback past three lines: %d rows, %q, %d decodes (want 1 + 3)", len(w.Agents), w.Degraded, decodes)
	}
}

// TestSeed_BigReplayBudget: a 50MiB replay (~100 large snapshots among
// 10k small frames) costs a bounded number of decodes and allocations.
// Not parallel: AllocsPerRun refuses to run in a parallel test.
func TestSeed_BigReplayBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 50MiB replay")
	}
	var lines []string
	size := 0
	for i := 0; size < 50<<20; i++ {
		task := fmt.Sprintf("wbig%05d", i%16)
		var line string
		if i%100 == 0 {
			line = string(bigSnapshot(398, bigOpts{task: task, running: 5, tick: i}))
		} else {
			line = smallProgress(task, i)
		}
		lines = append(lines, line)
		size += len(line)
	}
	var decodes int64
	allocs := testing.AllocsPerRun(1, func() {
		_, decodes = seed(t, lines, true)
	})
	if decodes > 16*4 {
		t.Errorf("%d decodes for 16 tasks, want ≤ 64", decodes)
	}
	t.Logf("%d lines, %d MiB: %d decodes, %.0f allocs", len(lines), size>>20, decodes, allocs)
	if allocs > 60_000 {
		t.Errorf("%.0f allocs, want ≤ 60k", allocs)
	}
}

// TestSeed_StatuslessPatchKeepsStatus: a task_updated patch holds only the
// fields that changed, so a newer one without a status must not hide the
// status an older one set. Seeded and live reads agree.
func TestSeed_StatuslessPatchKeepsStatus(t *testing.T) {
	t.Parallel()
	const task = "wpause001"
	upd := func(patch string) string {
		return fmt.Sprintf(`{"type":"system","subtype":"task_updated","task_id":%q,"patch":%s,"uuid":"u","session_id":%q}`, task, patch, probeSession)
	}
	lines := []string{
		fmt.Sprintf(`{"type":"system","subtype":"task_started","task_id":%q,"tool_use_id":"toolu_x","description":"d","task_type":"local_workflow","workflow_name":"n","uuid":"u","session_id":%q}`, task, probeSession),
		upd(`{"status":"paused"}`),
		upd(`{"description":"new desc"}`),
	}
	live := workflow.New(nil)
	feed(t, live, time.UnixMilli(1791170018000), lines...)
	tr, decodes := seed(t, lines, false)
	if got, want := only(t, tr).Status, only(t, live).Status; got != workflow.StatusPaused || got != want {
		t.Fatalf("seeded status %q, live %q, want paused", got, want)
	}
	if decodes != 3 {
		t.Errorf("%d decodes, want 3", decodes)
	}
	// Ended, then a patch without a status: the end time survives.
	lines = append(lines, upd(`{"status":"completed","end_time":42}`), upd(`{"is_backgrounded":true}`))
	tr, _ = seed(t, lines, false)
	if w := only(t, tr); w.Status != workflow.StatusCompleted || w.EndedAt != 42 {
		t.Fatalf("seeded terminal: %s ended %d", w.Status, w.EndedAt)
	}
}
