package attachment

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gcRefs is the test shim for the production GCWithRefs(ctx, ws, opts)
// signature with the standard 7d/30d TTLs. Returns removed count for
// the legacy (removed, err) assertion style most tests use.
func gcRefs(ws string, uploadTTL, refTTL time.Duration, now time.Time) (int, error) {
	res, err := GCWithRefs(context.Background(), ws, GCOptions{
		UploadTTL: uploadTTL,
		RefTTL:    refTTL,
		Now:       now,
	})
	return res.Removed, err
}

// TestMeta_AddReference_IdempotentSorted exercises the primary entry
// point the tracker uses. A bump of the same keyhash must not grow
// the slice or reorder it; distinct keyhashes must be kept sorted
// so operators eyeballing .meta files see a stable order.
func TestMeta_AddReference_IdempotentSorted(t *testing.T) {
	var m Meta
	if !m.AddReference("c") {
		t.Errorf("first bump reported no change")
	}
	if m.AddReference("c") {
		t.Errorf("duplicate bump reported change")
	}
	m.AddReference("a")
	m.AddReference("b")
	want := []string{"a", "b", "c"}
	if len(m.ReferencingKeyHashes) != len(want) {
		t.Fatalf("got %v, want %v", m.ReferencingKeyHashes, want)
	}
	for i, v := range want {
		if m.ReferencingKeyHashes[i] != v {
			t.Errorf("pos %d: %q, want %q", i, m.ReferencingKeyHashes[i], v)
		}
	}
}

// TestMeta_AddReference_EmptyNoOp: the tracker's bridge might call
// AddReference("") when an entry's ImagePaths slot is empty. The
// helper must not pollute the slice with empty strings.
func TestMeta_AddReference_EmptyNoOp(t *testing.T) {
	var m Meta
	if m.AddReference("") {
		t.Errorf("empty keyhash accepted")
	}
	if len(m.ReferencingKeyHashes) != 0 {
		t.Errorf("empty keyhash polluted slice: %v", m.ReferencingKeyHashes)
	}
}

// TestMeta_RemoveReference covers the symmetric path triggered by
// session deletion. The slice must shrink when the keyhash is present
// and stay untouched otherwise.
func TestMeta_RemoveReference(t *testing.T) {
	m := Meta{ReferencingKeyHashes: []string{"a", "b", "c"}}
	if !m.RemoveReference("b") {
		t.Errorf("remove middle reported no change")
	}
	if got := m.ReferencingKeyHashes; len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("unexpected slice: %v", got)
	}
	if m.RemoveReference("z") {
		t.Errorf("absent keyhash reported change")
	}
	if m.RemoveReference("") {
		t.Errorf("empty keyhash reported change")
	}
}

// TestMeta_HasReference is the pure-read variant the health index
// uses. Pin behaviour on boundaries.
func TestMeta_HasReference(t *testing.T) {
	m := Meta{ReferencingKeyHashes: []string{"a", "b", "c"}}
	for _, k := range []string{"a", "b", "c"} {
		if !m.HasReference(k) {
			t.Errorf("HasReference(%q) = false", k)
		}
	}
	for _, k := range []string{"", "z", "ba"} {
		if m.HasReference(k) {
			t.Errorf("HasReference(%q) = true, want false", k)
		}
	}
}

// TestMeta_JSONRoundTrip pins the wire shape so older naozhi builds
// that never encoded the new fields can still read what the new
// build writes (omitempty) and the reverse direction.
func TestMeta_JSONRoundTrip(t *testing.T) {
	// New fields set → survive round-trip.
	want := Meta{
		OrigName:             "foo.png",
		MimeType:             "image/png",
		Size:                 123,
		UploadedAt:           time.Unix(1700000000, 0).UTC(),
		ReferencingKeyHashes: []string{"a", "b"},
		LastReferencedAt:     1700000001000,
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Meta
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.ReferencingKeyHashes) != 2 || got.LastReferencedAt != 1700000001000 {
		t.Errorf("new fields dropped: %+v", got)
	}
	// Empty / zero new fields → omitted from wire.
	legacy := Meta{OrigName: "f.pdf", UploadedAt: time.Unix(1, 0)}
	raw2, _ := json.Marshal(legacy)
	if strings.Contains(string(raw2), "referencing_keyhashes") {
		t.Errorf("empty slice leaked to wire: %s", raw2)
	}
	if strings.Contains(string(raw2), "last_referenced_at") {
		t.Errorf("zero int leaked to wire: %s", raw2)
	}
}

// TestMeta_ReadsLegacyWithoutPanic is the forward-compat guard: a
// meta file produced by an older naozhi (no new fields) must be
// readable unchanged. GCWithRefs's legacy branch depends on this.
func TestMeta_ReadsLegacyWithoutPanic(t *testing.T) {
	raw := []byte(`{"orig_name":"f.png","mime_type":"image/png","size":10,"uploaded_at":"2026-01-01T00:00:00Z"}`)
	var m Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.ReferencingKeyHashes) != 0 {
		t.Errorf("unexpected refs: %v", m.ReferencingKeyHashes)
	}
	if m.LastReferencedAt != 0 {
		t.Errorf("unexpected last_ref: %d", m.LastReferencedAt)
	}
}

// fixturePersisted drops a fully-formed attachment into workspace so
// the GC tests exercise the real layout without going through Persist
// (avoids tangling workspace+dir validation for focused GC cases).
// Returns (payload, meta) absolute paths.
func fixturePersisted(t *testing.T, workspace, date, stem string, meta Meta) (string, string) {
	t.Helper()
	dayDir := filepath.Join(workspace, Dir, date)
	if err := os.MkdirAll(dayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(dayDir, stem+".png")
	if err := os.WriteFile(payload, []byte("fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(dayDir, stem+".meta")
	buf, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	return payload, metaPath
}

// TestGCWithRefs_KeepsReferenced: a file past upload TTL stays on
// disk as long as last_referenced_at is within refTTL.
func TestGCWithRefs_KeepsReferenced(t *testing.T) {
	ws := t.TempDir()
	now := time.Now().UTC()
	// Uploaded 10 days ago (> 7-day upload TTL) but referenced 1 day
	// ago (< 30-day refTTL).
	meta := Meta{
		UploadedAt:           now.AddDate(0, 0, -10),
		ReferencingKeyHashes: []string{"sess1"},
		LastReferencedAt:     now.AddDate(0, 0, -1).UnixMilli(),
	}
	payload, _ := fixturePersisted(t, ws, now.AddDate(0, 0, -10).Format("2006-01-02"), "keep1", meta)

	removed, err := gcRefs(ws, 7*24*time.Hour, 30*24*time.Hour, now)
	if err != nil {
		t.Fatalf("GCWithRefs: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed=%d, want 0 (reference within refTTL)", removed)
	}
	if _, err := os.Stat(payload); err != nil {
		t.Errorf("payload removed: %v", err)
	}
}

// TestGCWithRefs_RemovesExpiredAndUnreferenced covers the happy
// reap: both bounds elapsed.
func TestGCWithRefs_RemovesExpiredAndUnreferenced(t *testing.T) {
	ws := t.TempDir()
	now := time.Now().UTC()
	meta := Meta{UploadedAt: now.AddDate(0, 0, -10)} // no refs
	payload, metaPath := fixturePersisted(t, ws, now.AddDate(0, 0, -10).Format("2006-01-02"), "reap1", meta)

	removed, err := gcRefs(ws, 7*24*time.Hour, 30*24*time.Hour, now)
	if err != nil {
		t.Fatalf("GCWithRefs: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed=%d, want 1", removed)
	}
	if _, err := os.Stat(payload); !os.IsNotExist(err) {
		t.Errorf("payload still exists: err=%v", err)
	}
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Errorf("meta still exists: err=%v", err)
	}
}

// TestGCWithRefs_RemovesRefsElapsed: referenced sessions exist BUT
// their last observation is past refTTL — file is orphaned in
// practice and must be collected.
func TestGCWithRefs_RemovesRefsElapsed(t *testing.T) {
	ws := t.TempDir()
	now := time.Now().UTC()
	meta := Meta{
		UploadedAt:           now.AddDate(0, 0, -40),
		ReferencingKeyHashes: []string{"old-sess"},
		LastReferencedAt:     now.AddDate(0, 0, -35).UnixMilli(), // > 30-day refTTL
	}
	payload, _ := fixturePersisted(t, ws, now.AddDate(0, 0, -40).Format("2006-01-02"), "reap2", meta)

	removed, err := gcRefs(ws, 7*24*time.Hour, 30*24*time.Hour, now)
	if err != nil {
		t.Fatalf("GCWithRefs: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed=%d, want 1", removed)
	}
	if _, err := os.Stat(payload); !os.IsNotExist(err) {
		t.Errorf("payload still exists after double-expiry: %v", err)
	}
}

// TestGCWithRefs_KeepsFresh: within upload TTL wins even with no
// refs. Mirrors legacy behaviour for never-referenced recent uploads.
func TestGCWithRefs_KeepsFresh(t *testing.T) {
	ws := t.TempDir()
	now := time.Now().UTC()
	meta := Meta{UploadedAt: now.AddDate(0, 0, -1)} // 1 day old, no refs
	payload, _ := fixturePersisted(t, ws, now.AddDate(0, 0, -1).Format("2006-01-02"), "fresh", meta)

	removed, err := gcRefs(ws, 7*24*time.Hour, 30*24*time.Hour, now)
	if err != nil {
		t.Fatalf("GCWithRefs: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed=%d, want 0 (within upload TTL)", removed)
	}
	if _, err := os.Stat(payload); err != nil {
		t.Errorf("fresh payload removed: %v", err)
	}
}

// TestGCWithRefs_LegacyMetaNoNewFields: meta exists but predates the
// RFC (no new fields). Must behave like "no refs" and use the
// legacy upload-TTL only decision.
func TestGCWithRefs_LegacyMetaNoNewFields(t *testing.T) {
	ws := t.TempDir()
	now := time.Now().UTC()
	day := now.AddDate(0, 0, -10).Format("2006-01-02")
	dayDir := filepath.Join(ws, Dir, day)
	os.MkdirAll(dayDir, 0o700)
	payload := filepath.Join(dayDir, "legacy.png")
	os.WriteFile(payload, []byte("bytes"), 0o600)
	// Write a legacy-shape .meta (no new fields).
	metaPath := filepath.Join(dayDir, "legacy.meta")
	os.WriteFile(metaPath,
		[]byte(`{"orig_name":"l.png","uploaded_at":"`+now.AddDate(0, 0, -10).Format(time.RFC3339)+`"}`),
		0o600,
	)

	removed, err := gcRefs(ws, 7*24*time.Hour, 30*24*time.Hour, now)
	if err != nil {
		t.Fatalf("GCWithRefs: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed=%d, want 1 (legacy meta, past upload TTL)", removed)
	}
}

// TestGCWithRefs_MissingMeta: attachment file without a .meta
// sidecar falls back to the date-directory age heuristic, matching
// the legacy GC behaviour for migration.
func TestGCWithRefs_MissingMeta(t *testing.T) {
	ws := t.TempDir()
	now := time.Now().UTC()
	day := now.AddDate(0, 0, -20).Format("2006-01-02")
	dayDir := filepath.Join(ws, Dir, day)
	os.MkdirAll(dayDir, 0o700)
	payload := filepath.Join(dayDir, "no-meta.png")
	os.WriteFile(payload, []byte("b"), 0o600)
	// No .meta sibling.

	removed, err := gcRefs(ws, 7*24*time.Hour, 30*24*time.Hour, now)
	if err != nil {
		t.Fatalf("GCWithRefs: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed=%d, want 1 (no meta, past upload TTL)", removed)
	}
}

// TestGCWithRefs_PrunesEmptyDayDirs: after the per-file sweep, an
// old date directory with no survivors should be removed.
func TestGCWithRefs_PrunesEmptyDayDirs(t *testing.T) {
	ws := t.TempDir()
	now := time.Now().UTC()
	meta := Meta{UploadedAt: now.AddDate(0, 0, -10)}
	date := now.AddDate(0, 0, -10).Format("2006-01-02")
	fixturePersisted(t, ws, date, "reap", meta)

	_, err := gcRefs(ws, 7*24*time.Hour, 30*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(ws, Dir, date)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("empty day dir not pruned: err=%v", err)
	}
}

// TestGCWithRefs_WorkspaceEmpty returns 0, nil for a workspace
// without any attachments directory. Cron job starts there every
// tick on a fresh deployment.
func TestGCWithRefs_WorkspaceEmpty(t *testing.T) {
	ws := t.TempDir()
	removed, err := gcRefs(ws, 7*24*time.Hour, 30*24*time.Hour, time.Now())
	if err != nil {
		t.Fatalf("err on empty ws: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed=%d on empty ws", removed)
	}
}

// TestGCWithRefs_RequiresWorkspace rejects empty workspace with
// the documented sentinel.
func TestGCWithRefs_RequiresWorkspace(t *testing.T) {
	_, err := gcRefs("", time.Hour, time.Hour, time.Now())
	if !errors.Is(err, ErrWorkspaceRequired) {
		t.Errorf("err=%v, want ErrWorkspaceRequired", err)
	}
}

// TestUpdateMetaFile_Idempotent: apply a mutate that returns false
// → no rewrite, file mtime unchanged (we tolerate re-stat skew
// across filesystems by merely asserting no content change).
func TestUpdateMetaFile_Idempotent(t *testing.T) {
	ws := t.TempDir()
	_, metaPath := fixturePersisted(t, ws,
		time.Now().Format("2006-01-02"), "idem",
		Meta{UploadedAt: time.Now()},
	)
	orig, _ := os.ReadFile(metaPath)

	changed, err := UpdateMetaFile(metaPath, func(m *Meta) bool {
		// return false → no write
		return false
	})
	if err != nil {
		t.Fatalf("UpdateMetaFile: %v", err)
	}
	if changed {
		t.Errorf("changed=true despite mutate returning false")
	}
	got, _ := os.ReadFile(metaPath)
	if string(got) != string(orig) {
		t.Errorf("file rewritten despite no change")
	}
}

// TestUpdateMetaFile_AppliesMutation writes a bump and then reads
// the file back to confirm the change landed.
func TestUpdateMetaFile_AppliesMutation(t *testing.T) {
	ws := t.TempDir()
	_, metaPath := fixturePersisted(t, ws,
		time.Now().Format("2006-01-02"), "write",
		Meta{UploadedAt: time.Now()},
	)
	changed, err := UpdateMetaFile(metaPath, func(m *Meta) bool {
		m.AddReference("sess1")
		m.LastReferencedAt = 999
		return true
	})
	if err != nil {
		t.Fatalf("UpdateMetaFile: %v", err)
	}
	if !changed {
		t.Fatalf("changed=false despite mutation")
	}
	out, err := loadMetaFile(metaPath)
	if err != nil {
		t.Fatalf("load post-write: %v", err)
	}
	if !out.HasReference("sess1") {
		t.Errorf("reference not persisted: %+v", out)
	}
	if out.LastReferencedAt != 999 {
		t.Errorf("LastReferencedAt=%d, want 999", out.LastReferencedAt)
	}
}

// TestUpdateMetaFile_MissingSidecar refuses to write (we can't
// synthesise upload metadata). Tracker callers must check this and
// log a warning, not crash.
func TestUpdateMetaFile_MissingSidecar(t *testing.T) {
	changed, err := UpdateMetaFile(
		filepath.Join(t.TempDir(), "ghost.meta"),
		func(m *Meta) bool { return true },
	)
	if err == nil {
		t.Errorf("expected err on missing sidecar")
	}
	if changed {
		t.Errorf("changed=true for missing sidecar")
	}
}
