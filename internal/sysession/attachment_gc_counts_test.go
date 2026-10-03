package sysession

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/attachment"
)

// seedGCFixture drops one past-upload-TTL payload of body into
// <ws>/.naozhi/attachments/<10d-ago>/ with meta as its sidecar (nil = legacy,
// no sidecar). Returns the payload path.
func seedGCFixture(t *testing.T, ws, stem, body string, meta *attachment.Meta, now time.Time) string {
	t.Helper()
	dir := filepath.Join(ws, attachment.Dir, now.AddDate(0, 0, -10).Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(dir, stem+".png")
	if err := os.WriteFile(payload, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if meta != nil {
		buf, _ := json.Marshal(meta)
		if err := os.WriteFile(filepath.Join(dir, stem+".meta"), buf, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return payload
}

// seedThreeBuckets gives two roots one fixture per reap reason between them:
// ws1 holds legacy (3 B) + no-refs (5 B), ws2 holds no-refs (7 B) + expired (11 B).
func seedThreeBuckets(t *testing.T, now time.Time) (ws1, ws2 string, payloads []string) {
	t.Helper()
	ws1, ws2 = t.TempDir(), t.TempDir()
	noRefs := &attachment.Meta{UploadedAt: now.AddDate(0, 0, -10)}
	expired := &attachment.Meta{
		UploadedAt:           now.AddDate(0, 0, -40),
		ReferencingKeyHashes: []string{"s"},
		LastReferencedAt:     now.AddDate(0, 0, -35).UnixMilli(),
	}
	payloads = []string{
		seedGCFixture(t, ws1, "legacy", "abc", nil, now),
		seedGCFixture(t, ws1, "norefs1", "12345", noRefs, now),
		seedGCFixture(t, ws2, "norefs2", "1234567", noRefs, now),
		seedGCFixture(t, ws2, "expired", "12345678901", expired, now),
	}
	return ws1, ws2, payloads
}

// TestAttachmentGC_DryRunReportsWouldReapCounts: a dry-run tick reports the
// per-bucket sums and the byte total across roots in Counts, flags the mode,
// deletes nothing, and the flattened Stats carry the keys verbatim.
func TestAttachmentGC_DryRunReportsWouldReapCounts(t *testing.T) {
	now := time.Now().UTC()
	ws1, ws2, payloads := seedThreeBuckets(t, now)

	gc := newTestGC(fakeRoots{[]string{ws1, ws2}}, now)
	gc.dryRun = true
	rep, err := gc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	want := map[string]int64{
		gcCountDryRun:       1,
		gcCountLegacyNoMeta: 1,
		gcCountMetaNoRefs:   2,
		gcCountRefsExpired:  1,
		gcCountBytes:        3 + 5 + 7 + 11,
	}
	for k, v := range want {
		if got := rep.Counts[k]; got != v {
			t.Errorf("Counts[%q]=%d, want %d", k, got, v)
		}
	}
	if len(rep.Counts) != len(want) {
		t.Errorf("Counts=%v, want exactly the keys %v", rep.Counts, want)
	}
	for _, p := range payloads {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("dry-run deleted %s: %v", p, err)
		}
	}
	stats := flattenTickReport(rep)
	for k, v := range want {
		if stats[k] != v {
			t.Errorf("Stats[%q]=%d, want %d (Counts copied verbatim)", k, stats[k], v)
		}
	}
	if stats["examined"] != 2 {
		t.Errorf("Stats[examined]=%d, want 2 (Counts must not clobber the base keys)", stats["examined"])
	}
}

// TestAttachmentGC_DryRunNothingToReapShowsZeroBytes: a dry-run tick that
// finds nothing still reports the mode and a zero byte total, so the card
// distinguishes "measured, nothing to reclaim" from "never measured".
func TestAttachmentGC_DryRunNothingToReapShowsZeroBytes(t *testing.T) {
	gc := newTestGC(fakeRoots{[]string{t.TempDir()}}, time.Now().UTC())
	gc.dryRun = true
	rep, err := gc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if v, ok := rep.Counts[gcCountBytes]; !ok || v != 0 {
		t.Errorf("Counts[%q]=%d (present=%v), want 0 present", gcCountBytes, v, ok)
	}
	if rep.Counts[gcCountDryRun] != 1 {
		t.Errorf("Counts[%q]=%d, want 1", gcCountDryRun, rep.Counts[gcCountDryRun])
	}
}

// TestAttachmentGC_LiveCountsHaveNoDryRunKey: live mode reports the reasons
// behind its deletions but never the dry_run flag.
func TestAttachmentGC_LiveCountsHaveNoDryRunKey(t *testing.T) {
	now := time.Now().UTC()
	ws1, ws2, _ := seedThreeBuckets(t, now)

	gc := newTestGC(fakeRoots{[]string{ws1, ws2}}, now)
	rep, err := gc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if _, ok := rep.Counts[gcCountDryRun]; ok {
		t.Errorf("live tick reported %q: %v", gcCountDryRun, rep.Counts)
	}
	if rep.Acted != 4 || rep.Counts[gcCountMetaNoRefs] != 2 || rep.Counts[gcCountBytes] != 26 {
		t.Errorf("Acted=%d Counts=%v, want 4 removals, meta_no_refs=2, bytes=26", rep.Acted, rep.Counts)
	}
}

// TestAttachmentGC_CountKeysAvoidReservedStats: no Counts key may collide
// with the names flattenTickReport derives from Examined / Acted / Skipped.
func TestAttachmentGC_CountKeysAvoidReservedStats(t *testing.T) {
	for _, k := range []string{gcCountDryRun, gcCountBytes, gcCountLegacyNoMeta, gcCountMetaNoRefs, gcCountRefsExpired} {
		if k == "examined" || k == "acted" || strings.HasPrefix(k, "skipped_") {
			t.Errorf("Counts key %q collides with a reserved Stats name", k)
		}
	}
}

// TestAttachmentGC_SweepSummaryLog: each root with something to reap logs one
// summary line carrying its buckets and bytes; a root with nothing logs none.
func TestAttachmentGC_SweepSummaryLog(t *testing.T) {
	now := time.Now().UTC()
	ws1, ws2, _ := seedThreeBuckets(t, now)
	empty := t.TempDir()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	gc := newTestGC(fakeRoots{[]string{ws1, ws2, empty}}, now)
	gc.dryRun = true
	if _, err := gc.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, `msg="attachment-gc: sweep summary"`) {
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("got %d summary lines, want 2 (one per root with work):\n%s", len(lines), buf.String())
	}
	for _, frag := range []string{"root=" + ws2, "dry_run=true", "files=2", "bytes=18",
		"legacy_no_meta=0", "meta_no_refs=1", "refs_expired=1", "removed=0", "cap_hit=false"} {
		if !strings.Contains(lines[1], frag) {
			t.Errorf("ws2 summary line lacks %q: %s", frag, lines[1])
		}
	}
}
