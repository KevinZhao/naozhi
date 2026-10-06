package runstore

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// TestInflightMarker_WireFormatGolden pins the marker's JSON: a marker written
// by the previous binary is what the next boot reconciles, so the field names
// must not drift across an upgrade. Both constants were generated from the
// type as it stood in internal/cron before it moved here.
func TestInflightMarker_WireFormatGolden(t *testing.T) {
	t.Parallel()
	const wantFull = `{"job_id":"0123456789abcdef","run_id":"feedfacefeedface","trigger":"manual","started_at_ms":1700000000123,"prompt":"do thing","work_dir":"/tmp/wd","fresh":true,"attempts":1,"adopt_after":"4242:9"}`
	const wantMin = `{"job_id":"0123456789abcdef","run_id":"feedfacefeedface","started_at_ms":1}`
	full := InflightMarker{
		JobID: "0123456789abcdef", RunID: "feedfacefeedface", Trigger: runtelemetry.TriggerManual,
		StartedAtMS: 1700000000123, Prompt: "do thing", WorkDir: "/tmp/wd", Fresh: true,
		Attempts: 1, SendWatermark: "4242:9",
	}
	for name, tc := range map[string]struct {
		m    InflightMarker
		want string
	}{
		"full":    {full, wantFull},
		"minimal": {InflightMarker{JobID: "0123456789abcdef", RunID: "feedfacefeedface", StartedAtMS: 1}, wantMin},
	} {
		b, err := json.Marshal(tc.m)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(b) != tc.want {
			t.Errorf("%s marker JSON drifted:\n got %s\nwant %s", name, b, tc.want)
		}
	}
}

func TestMarkers_WriteReadRewrite(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ms := Markers{Root: root}
	m := InflightMarker{
		JobID: mustGenerateID(), RunID: mustGenerateRunID(), Trigger: runtelemetry.TriggerScheduled,
		StartedAtMS: 1700000000000, Prompt: "p", WorkDir: "/wd", Fresh: true,
	}
	path := ms.Write(m, slog.Default())
	if want := filepath.Join(root, "runinflight", m.RunID+".json"); path != want {
		t.Fatalf("Write path = %q, want %q", path, want)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("marker mode = %v, want 0600", perm)
		}
	}
	got, ok := ms.Read(path)
	if !ok || got != m {
		t.Fatalf("Read = (%+v, %v), want (%+v, true)", got, ok, m)
	}

	m.Attempts, m.SendWatermark = 1, "4242:9"
	if !ms.Rewrite(path, m) {
		t.Fatal("Rewrite reported failure")
	}
	if got, ok := ms.Read(path); !ok || got != m {
		t.Errorf("after Rewrite Read = (%+v, %v), want (%+v, true)", got, ok, m)
	}
}

// TestMarkers_ZeroValueIsDisabled: a store-less scheduler has an empty root,
// and nothing may then be written relative to the working directory.
func TestMarkers_ZeroValueIsDisabled(t *testing.T) {
	t.Parallel()
	var ms Markers
	if d := ms.Dir(); d != "" {
		t.Errorf("Dir = %q, want empty", d)
	}
	if p := ms.Write(InflightMarker{JobID: "a", RunID: "b", StartedAtMS: 1}, slog.Default()); p != "" {
		t.Errorf("Write = %q, want empty", p)
	}
	ms.Remove("b")
	if entries, err := ms.List(); entries != nil || err != nil {
		t.Errorf("List = (%v, %v), want (nil, nil)", entries, err)
	}
}

// TestMarkers_WriteRefusesSymlinkedDir: a planted runinflight symlink must not
// redirect the write, nor where the next boot looks (#2166).
func TestMarkers_WriteRefusesSymlinkedDir(t *testing.T) {
	t.Parallel()
	root, elsewhere := t.TempDir(), t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(root, "runinflight")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	ms := Markers{Root: root}
	if p := ms.Write(InflightMarker{JobID: mustGenerateID(), RunID: mustGenerateRunID(), StartedAtMS: 1}, slog.Default()); p != "" {
		t.Errorf("Write through a symlinked dir = %q, want refused", p)
	}
	entries, err := os.ReadDir(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("symlink target holds %d entries, want none", len(entries))
	}
}

func TestMarkers_ReadRejectsUnusable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ms := Markers{Root: dir}
	for name, body := range map[string]string{
		"corrupt":   `{not json`,
		"nojobid":   `{"run_id":"aaaa","started_at_ms":1}`,
		"norunid":   `{"job_id":"bbbb","started_at_ms":1}`,
		"nostarted": `{"job_id":"bbbb","run_id":"aaaa"}`,
		"negstart":  `{"job_id":"bbbb","run_id":"aaaa","started_at_ms":-5}`,
	} {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if m, ok := ms.Read(path); ok {
			t.Errorf("%s: Read = (%+v, true), want unusable", name, m)
		}
	}
	if _, ok := ms.Read(filepath.Join(dir, "missing.json")); ok {
		t.Error("missing file: Read ok, want unusable")
	}
}

// TestMarkers_ReadAcceptsOlderMarkers: a marker from a binary that predates
// adoption has neither attempts nor adopt_after, and still parses.
func TestMarkers_ReadAcceptsOlderMarkers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "old.json")
	if err := os.WriteFile(path, []byte(`{"job_id":"j1","run_id":"r1","started_at_ms":1700000000000}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, ok := Markers{Root: dir}.Read(path)
	if !ok || m.SendWatermark != "" || m.Attempts != 0 || m.JobID != "j1" {
		t.Errorf("old marker = (%+v, %v), want parsed with no watermark and no attempts", m, ok)
	}
}

func TestMarkers_List(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ms := Markers{Root: root}
	if entries, err := ms.List(); entries != nil || err != nil {
		t.Fatalf("List before any write = (%v, %v), want (nil, nil)", entries, err)
	}
	good := InflightMarker{JobID: mustGenerateID(), RunID: mustGenerateRunID(), StartedAtMS: 1}
	goodPath := ms.Write(good, slog.Default())
	if goodPath == "" {
		t.Fatal("Write failed")
	}
	dir := ms.Dir()
	if err := os.WriteFile(filepath.Join(dir, "corrupt.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte(`x`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	entries, err := ms.List()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]InflightEntry{}
	for _, e := range entries {
		got[filepath.Base(e.Path)] = e
	}
	if len(got) != 2 {
		t.Fatalf("List = %+v, want exactly the two *.json files", entries)
	}
	if e := got[good.RunID+".json"]; !e.OK || e.Marker != good || e.Path != goodPath {
		t.Errorf("good entry = %+v, want OK with %+v at %q", e, good, goodPath)
	}
	if e, ok := got["corrupt.json"]; !ok || e.OK {
		t.Errorf("corrupt entry = %+v (present %v), want present and not OK", e, ok)
	}
}

func TestMarkers_ListScanError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "runinflight"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if entries, err := (Markers{Root: root}).List(); err == nil {
		t.Errorf("List over a non-directory = (%v, nil), want an error", entries)
	}
}

func TestMarkers_Remove(t *testing.T) {
	t.Parallel()
	ms := Markers{Root: t.TempDir()}
	a := InflightMarker{JobID: mustGenerateID(), RunID: mustGenerateRunID(), StartedAtMS: 1}
	b := InflightMarker{JobID: mustGenerateID(), RunID: mustGenerateRunID(), StartedAtMS: 1}
	pa, pb := ms.Write(a, slog.Default()), ms.Write(b, slog.Default())

	ms.Remove(a.RunID)
	if _, err := os.Stat(pa); !os.IsNotExist(err) {
		t.Errorf("Remove(runID) left the marker: %v", err)
	}
	ms.Remove(a.RunID) // already gone: no-op

	if err := ms.RemovePath(pb); err != nil {
		t.Fatalf("RemovePath = %v", err)
	}
	if _, err := os.Stat(pb); !os.IsNotExist(err) {
		t.Errorf("RemovePath left the marker: %v", err)
	}
	if err := ms.RemovePath(pb); err != nil {
		t.Errorf("RemovePath of a missing file = %v, want nil", err)
	}

	full := filepath.Join(ms.Dir(), "full.json")
	if err := os.Mkdir(full, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ms.RemovePath(full); err == nil {
		t.Error("RemovePath of a non-empty directory = nil, want the error surfaced")
	}
}
