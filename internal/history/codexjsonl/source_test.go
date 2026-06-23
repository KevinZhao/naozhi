package codexjsonl

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// writeRollout creates a date-bucketed rollout file for sid under root and
// returns nothing — codex names files YYYY/MM/DD/rollout-<iso>-<sid>.jsonl.
func writeRollout(t *testing.T, root, sid string, lines []string) {
	t.Helper()
	dir := filepath.Join(root, "2026", "06", "21")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	name := "rollout-2026-06-21T09-35-20-" + sid + ".jsonl"
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
}

func TestSource_ImplementsInterface(t *testing.T) {
	t.Parallel()
	var _ cli.HistorySource = (*Source)(nil)
}

func TestSource_LoadBefore_FullRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "019ee988-da7f-7821-b6d1-7b74a7db62d6"
	writeRollout(t, dir, sid, []string{
		`{"timestamp":"2026-06-21T09:35:20.700Z","type":"session_meta","payload":{"id":"x"}}`,
		`{"timestamp":"2026-06-21T09:35:21.000Z","type":"event_msg","payload":{"type":"user_message","message":"hello codex"}}`,
		`{"timestamp":"2026-06-21T09:35:22.500Z","type":"event_msg","payload":{"type":"token_count","message":""}}`,
		`{"timestamp":"2026-06-21T09:35:23.000Z","type":"event_msg","payload":{"type":"agent_message","message":"hi there"}}`,
		`{"timestamp":"2026-06-21T09:35:24.000Z","type":"response_item","payload":{"type":"message"}}`,
	})

	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 (user + agent only): %+v", len(got), got)
	}
	if got[0].Type != "user" || got[0].Summary != "hello codex" {
		t.Errorf("entry[0] = %+v; want user/hello codex", got[0])
	}
	if got[1].Type != "text" || got[1].Summary != "hi there" {
		t.Errorf("entry[1] = %+v; want text/hi there", got[1])
	}
	// Chronological order via real ISO timestamps.
	if !(got[0].Time < got[1].Time) {
		t.Errorf("entries not chronological: %d >= %d", got[0].Time, got[1].Time)
	}
}

func TestSource_LoadBefore_Limit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "lim-thread"
	writeRollout(t, dir, sid, []string{
		`{"timestamp":"2026-06-21T09:35:21.000Z","type":"event_msg","payload":{"type":"user_message","message":"m1"}}`,
		`{"timestamp":"2026-06-21T09:35:22.000Z","type":"event_msg","payload":{"type":"agent_message","message":"m2"}}`,
		`{"timestamp":"2026-06-21T09:35:23.000Z","type":"event_msg","payload":{"type":"user_message","message":"m3"}}`,
	})
	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 2)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (newest tail)", len(got))
	}
	// Newest two: m2, m3.
	if got[0].Summary != "m2" || got[1].Summary != "m3" {
		t.Errorf("got %q,%q; want m2,m3", got[0].Summary, got[1].Summary)
	}
}

func TestSource_LoadBefore_BeforeFiltering(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "before-thread"
	writeRollout(t, dir, sid, []string{
		`{"timestamp":"2026-06-21T09:35:21.000Z","type":"event_msg","payload":{"type":"user_message","message":"old"}}`,
		`{"timestamp":"2026-06-21T09:35:25.000Z","type":"event_msg","payload":{"type":"agent_message","message":"new"}}`,
	})
	src := New(dir, func() string { return sid })
	// beforeMS just after the first entry → only "old" survives.
	oldMS := int64(1782034521000 + 1) // approx; compute from actual below
	// Recompute precisely: 2026-06-21T09:35:21Z.
	_ = oldMS
	full, _ := src.LoadBefore(context.Background(), 0, 10)
	if len(full) != 2 {
		t.Fatalf("precondition: want 2 entries, got %d", len(full))
	}
	cut := full[1].Time // == "new" time; strictly-< drops it
	got, err := src.LoadBefore(context.Background(), cut, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 1 || got[0].Summary != "old" {
		t.Fatalf("before-filter got %+v; want only 'old'", got)
	}
}

func TestSource_LoadBefore_RejectsPathTraversal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, sid := range []string{"../etc/passwd", "a/b", `a\b`, ".."} {
		src := New(dir, func() string { return sid })
		got, err := src.LoadBefore(context.Background(), 0, 10)
		if err != nil || got != nil {
			t.Errorf("sid %q: got (%v,%v); want (nil,nil)", sid, got, err)
		}
	}
}

func TestSource_LoadBefore_MissingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := New(dir, func() string { return "no-such-thread" })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil || got != nil {
		t.Errorf("got (%v,%v); want (nil,nil) for missing rollout", got, err)
	}
}

func TestSource_LoadBefore_DegradedStates(t *testing.T) {
	t.Parallel()
	// nil sessionID fn / empty rootDir / empty sid all degrade to (nil,nil).
	cases := []*Source{
		New("", func() string { return "x" }),
		New("/tmp", nil),
		New("/tmp", func() string { return "" }),
	}
	for i, src := range cases {
		got, err := src.LoadBefore(context.Background(), 0, 10)
		if err != nil || got != nil {
			t.Errorf("case %d: got (%v,%v); want (nil,nil)", i, got, err)
		}
	}
	// limit <= 0 short-circuits.
	src := New("/tmp", func() string { return "x" })
	if got, err := src.LoadBefore(context.Background(), 0, 0); got != nil || err != nil {
		t.Errorf("limit 0: got (%v,%v); want (nil,nil)", got, err)
	}
}

func TestSource_LoadBefore_MalformedLinesSkipped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "malformed-thread"
	writeRollout(t, dir, sid, []string{
		`{not json`,
		``,
		`{"timestamp":"2026-06-21T09:35:21.000Z","type":"event_msg","payload":{"type":"user_message","message":"survivor"}}`,
		`{"timestamp":"bad-ts","type":"event_msg","payload":{"type":"agent_message","message":"dropped-no-ts"}}`,
		`{"timestamp":"2026-06-21T09:35:23.000Z","type":"event_msg","payload":{"type":"agent_message","message":"   "}}`,
	})
	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 1 || got[0].Summary != "survivor" {
		t.Fatalf("got %+v; want only 'survivor' (bad ts + blank text dropped)", got)
	}
}

func TestFactory_DegradesWithoutDir(t *testing.T) {
	t.Parallel()
	got := factory(stubView{sid: "x"}, cli.HistoryWiring{}) // no CodexSessionsDir
	if _, ok := got.(cli.NoopHistorySource); !ok {
		t.Errorf("factory without CodexSessionsDir = %T; want NoopHistorySource", got)
	}
	got2 := factory(stubView{sid: "x"}, cli.HistoryWiring{CodexSessionsDir: "/tmp"})
	if _, ok := got2.(*Source); !ok {
		t.Errorf("factory with CodexSessionsDir = %T; want *Source", got2)
	}
}

func TestParseISOms(t *testing.T) {
	t.Parallel()
	if _, ok := parseISOms(""); ok {
		t.Error("empty ts should fail")
	}
	if _, ok := parseISOms("not-a-time"); ok {
		t.Error("garbage ts should fail")
	}
	ms, ok := parseISOms("2026-06-21T09:35:21.000Z")
	if !ok || ms <= 0 {
		t.Errorf("valid ts parse failed: ms=%d ok=%v", ms, ok)
	}
}

// TestSource_LoadBefore_SetsDedupUUID pins that every surfaced entry carries
// a non-empty, deterministic UUID so merged.Source can dedup overlapping
// pages. Without it the same codex line renders twice whenever a LoadBefore
// cursor straddles a previously-returned entry — claude and kiro both set
// this; codex must match the contract.
func TestSource_LoadBefore_SetsDedupUUID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "uuid-thread"
	writeRollout(t, dir, sid, []string{
		`{"timestamp":"2026-06-21T09:35:21.000Z","type":"event_msg","payload":{"type":"user_message","message":"q1"}}`,
		`{"timestamp":"2026-06-21T09:35:23.000Z","type":"event_msg","payload":{"type":"agent_message","message":"a1"}}`,
	})
	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	for i, e := range got {
		if e.UUID == "" {
			t.Errorf("entry[%d] (%s/%q) has empty UUID — merged.Source cannot dedup it", i, e.Type, e.Summary)
		}
	}
	if got[0].UUID == got[1].UUID {
		t.Errorf("distinct entries share a UUID %q — dedup would drop one", got[0].UUID)
	}
	// Determinism: re-reading the same file yields the same UUIDs (so a
	// re-fetched overlapping page dedups against the first fetch).
	again, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore (2nd): %v", err)
	}
	if len(again) != 2 || again[0].UUID != got[0].UUID || again[1].UUID != got[1].UUID {
		t.Errorf("UUIDs not stable across reads: %v vs %v", []string{got[0].UUID, got[1].UUID}, []string{again[0].UUID, again[1].UUID})
	}
}

// TestSource_LoadBefore_AlreadyOrderedPreserved pins that an in-order rollout
// (codex's normal append contract) round-trips unchanged — the IsSorted
// fast-path must be behaviour-equivalent to an unconditional stable sort.
func TestSource_LoadBefore_AlreadyOrderedPreserved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "ordered-thread"
	writeRollout(t, dir, sid, []string{
		`{"timestamp":"2026-06-21T09:35:21.000Z","type":"event_msg","payload":{"type":"user_message","message":"a"}}`,
		`{"timestamp":"2026-06-21T09:35:22.000Z","type":"event_msg","payload":{"type":"agent_message","message":"b"}}`,
		`{"timestamp":"2026-06-21T09:35:23.000Z","type":"event_msg","payload":{"type":"user_message","message":"c"}}`,
	})
	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Summary != w {
			t.Errorf("entry[%d] = %q; want %q", i, got[i].Summary, w)
		}
	}
}

// TestSource_LoadBefore_OutOfOrderSorted pins that the defensive fallback still
// sorts a rollout whose timestamps arrive out of order, so the IsSorted
// fast-path never leaves an unsorted result for downstream pagination.
func TestSource_LoadBefore_OutOfOrderSorted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "unordered-thread"
	writeRollout(t, dir, sid, []string{
		`{"timestamp":"2026-06-21T09:35:23.000Z","type":"event_msg","payload":{"type":"user_message","message":"late"}}`,
		`{"timestamp":"2026-06-21T09:35:21.000Z","type":"event_msg","payload":{"type":"agent_message","message":"early"}}`,
		`{"timestamp":"2026-06-21T09:35:22.000Z","type":"event_msg","payload":{"type":"user_message","message":"mid"}}`,
	})
	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	want := []string{"early", "mid", "late"}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Summary != w {
			t.Errorf("entry[%d] = %q; want %q (out-of-order input must be sorted)", i, got[i].Summary, w)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Time > got[i].Time {
			t.Errorf("result not sorted at %d: %d > %d", i, got[i-1].Time, got[i].Time)
		}
	}
}

// stubView is a minimal cli.HistorySessionView for factory tests.
type stubView struct{ sid string }

func (s stubView) SessionKey() string         { return "k" }
func (s stubView) Workspace() string          { return "/tmp" }
func (s stubView) SessionID() string          { return s.sid }
func (s stubView) SnapshotChainIDs() []string { return nil }
