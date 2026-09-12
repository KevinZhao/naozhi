package datadir

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/testhelper"
)

func writeAged(t *testing.T, dir, name string, body string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	at := time.Now().Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
	return path
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// TestPassRemovesOnlyOldFilesOfItsExtension carries over what
// sysession.SweepOldJSONL's test pinned: age decides, and the extension filter
// keeps the sweep off files naozhi did not name.
func TestPassRemovesOnlyOldFilesOfItsExtension(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	oldJSONL := writeAged(t, dir, "a.jsonl", "x", 30*24*time.Hour)
	freshJSONL := writeAged(t, dir, "b.jsonl", "x", time.Minute)
	oldTxt := writeAged(t, dir, "c.txt", "x", 30*24*time.Hour)
	oldJSON := writeAged(t, dir, "d.json", "x", 30*24*time.Hour)

	res, err := Pass{Name: "t", Dir: dir, Ext: ".jsonl", MaxAge: 7 * 24 * time.Hour}.Run()
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	if res.Removed != 1 || res.Bytes != 1 {
		t.Errorf("Result = %+v, want {Removed:1 Bytes:1}", res)
	}
	if exists(t, oldJSONL) {
		t.Error("old .jsonl should be gone")
	}
	for _, keep := range []string{freshJSONL, oldTxt, oldJSON} {
		if !exists(t, keep) {
			t.Errorf("%s should have been kept", filepath.Base(keep))
		}
	}
}

// TestPassNoOpInputs: an empty dir, a missing dir, and a non-positive MaxAge are
// all no-ops rather than errors — callers pass config values through unguarded,
// and "0 disables" is the existing convention for jsonl_max_age.
func TestPassNoOpInputs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	kept := writeAged(t, dir, "old.jsonl", "x", 30*24*time.Hour)

	cases := []struct {
		name string
		p    Pass
	}{
		{"empty dir", Pass{Name: "t", Dir: "", Ext: ".jsonl", MaxAge: time.Hour}},
		{"missing dir", Pass{Name: "t", Dir: filepath.Join(dir, "nope"), Ext: ".jsonl", MaxAge: time.Hour}},
		{"zero MaxAge", Pass{Name: "t", Dir: dir, Ext: ".jsonl", MaxAge: 0}},
		{"negative MaxAge", Pass{Name: "t", Dir: dir, Ext: ".jsonl", MaxAge: -time.Hour}},
		{"empty Ext", Pass{Name: "t", Dir: dir, Ext: "", MaxAge: time.Hour}},
	}
	for _, c := range cases {
		res, err := c.p.Run()
		if err != nil {
			t.Errorf("%s: err = %v, want nil", c.name, err)
		}
		if res.Removed != 0 {
			t.Errorf("%s: removed %d, want 0", c.name, res.Removed)
		}
	}
	if !exists(t, kept) {
		t.Error("no-op passes must not remove anything")
	}
}

// TestPassKeepPredicateWins pins the "still in use" escape hatch: only the owning
// package can answer it, so Keep is consulted for every file already past MaxAge.
func TestPassKeepPredicateWins(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	live := writeAged(t, dir, "shim-1.log", "x", 30*24*time.Hour)
	dead := writeAged(t, dir, "shim-2.log", "x", 30*24*time.Hour)

	var asked []string
	res, err := Pass{
		Name: "t", Dir: dir, Ext: ".log", MaxAge: time.Hour,
		Keep: func(name string) bool {
			asked = append(asked, name)
			return name == "shim-1.log"
		},
	}.Run()
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	if res.Removed != 1 {
		t.Errorf("removed = %d, want 1", res.Removed)
	}
	if !exists(t, live) {
		t.Error("Keep returned true; file must survive")
	}
	if exists(t, dead) {
		t.Error("Keep returned false; file should be gone")
	}
	if len(asked) != 2 {
		t.Errorf("Keep consulted for %v, want both over-age files", asked)
	}
}

// TestPassSkipsFreshFilesBeforeConsultingKeep: Keep is an escape hatch for
// over-age files, not a per-file veto on every entry — a fresh file must never
// even be offered, so a pass whose Keep is expensive stays cheap.
func TestPassSkipsFreshFilesBeforeConsultingKeep(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeAged(t, dir, "fresh.log", "x", time.Minute)
	writeAged(t, dir, "stale.log", "x", 30*24*time.Hour)

	var asked []string
	if _, err := (Pass{
		Name: "t", Dir: dir, Ext: ".log", MaxAge: time.Hour,
		Keep: func(name string) bool { asked = append(asked, name); return false },
	}).Run(); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != "stale.log" {
		t.Errorf("Keep consulted for %v, want only [stale.log]", asked)
	}
}

// TestPassRemovesOnlyRegularFiles: os.Remove would happily delete a socket, fifo
// or device node sitting in a swept directory, so the sweep checks the mode. A
// socket is the case that discriminates — unlike a symlink, whose removal would
// only unlink the link and never reach its target.
func TestPassRemovesOnlyRegularFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "live.log")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	defer ln.Close()
	at := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(sockPath, at, at); err != nil {
		t.Skipf("cannot age the socket node: %v", err)
	}
	// Sanity: the node IS past the cutoff, so only the mode check can save it.
	info, err := os.Lstat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().IsRegular() {
		t.Fatalf("fixture is a regular file (mode %v); the test would not discriminate", info.Mode())
	}
	if !info.ModTime().Before(time.Now().Add(-time.Hour)) {
		t.Fatalf("fixture mtime %v is not past the cutoff; the age check would skip it", info.ModTime())
	}

	res, err := Pass{Name: "t", Dir: dir, Ext: ".log", MaxAge: time.Hour}.Run()
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	if res.Removed != 0 {
		t.Errorf("removed %d; a socket is not a regular file", res.Removed)
	}
	if !exists(t, sockPath) {
		t.Error("the sweep deleted a socket node")
	}
}

// TestPassIgnoresSubdirectories: the sweep is deliberately shallow, so a tree a
// future feature nests here is not silently pruned by an unrelated pass.
func TestPassIgnoresSubdirectories(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sub := filepath.Join(dir, "nested.log")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	inner := writeAged(t, sub, "deep.log", "x", 30*24*time.Hour)
	at := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(sub, at, at); err != nil {
		t.Fatal(err)
	}
	res, err := Pass{Name: "t", Dir: dir, Ext: ".log", MaxAge: time.Hour}.Run()
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	if res.Removed != 0 {
		t.Errorf("removed %d; directories are out of scope", res.Removed)
	}
	if !exists(t, inner) || !exists(t, sub) {
		t.Error("a subdirectory and its contents must be untouched")
	}
}

// TestSweeperRunOnceAggregatesAndReportsPerPass: Totals is what an operator-facing
// report reads, so it must accumulate across passes rather than show the last one.
func TestSweeperRunOnceAggregatesAndReportsPerPass(t *testing.T) {
	t.Parallel()
	a, b := t.TempDir(), t.TempDir()
	writeAged(t, a, "1.log", "aa", 30*24*time.Hour)
	writeAged(t, a, "2.log", "bbb", 30*24*time.Hour)
	writeAged(t, b, "3.jsonl", "c", 30*24*time.Hour)

	s := NewSweeper(0)
	s.Add(Pass{Name: "logs", Dir: a, Ext: ".log", MaxAge: time.Hour})
	s.Add(Pass{Name: "jsonl", Dir: b, Ext: ".jsonl", MaxAge: time.Hour})

	got := s.RunOnce()
	if got["logs"] != (Result{Removed: 2, Bytes: 5}) {
		t.Errorf(`got["logs"] = %+v, want {2 5}`, got["logs"])
	}
	if got["jsonl"] != (Result{Removed: 1, Bytes: 1}) {
		t.Errorf(`got["jsonl"] = %+v, want {1 1}`, got["jsonl"])
	}

	// A second pass finds nothing, but the running totals must not reset.
	if second := s.RunOnce(); second["logs"].Removed != 0 {
		t.Errorf("second pass removed %d, want 0", second["logs"].Removed)
	}
	tot := s.Totals()
	if tot["logs"] != (Result{Removed: 2, Bytes: 5}) {
		t.Errorf(`Totals()["logs"] = %+v, want the cumulative {2 5}`, tot["logs"])
	}
}

// TestSweeperRunSweepsImmediately: the sys-sessions tree used to be swept once at
// startup. Registering it must not lose that — Run does a pass before waiting on
// the first tick.
func TestSweeperRunSweepsImmediately(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	old := writeAged(t, dir, "a.jsonl", "x", 30*24*time.Hour)

	s := NewSweeper(time.Hour) // first tick an hour out; only the immediate pass can act
	s.Add(Pass{Name: "jsonl", Dir: dir, Ext: ".jsonl", MaxAge: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	testhelper.Eventually(t, func() bool { return !exists(t, old) }, 5*time.Second,
		"Run did not sweep before its first tick")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on ctx cancel")
	}
}

// TestSweeperTotalsIsACopy: a caller must not be able to mutate the Sweeper's
// bookkeeping through the returned map.
func TestSweeperTotalsIsACopy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeAged(t, dir, "a.log", "x", 30*24*time.Hour)
	s := NewSweeper(0)
	s.Add(Pass{Name: "logs", Dir: dir, Ext: ".log", MaxAge: time.Hour})
	s.RunOnce()

	tot := s.Totals()
	tot["logs"] = Result{Removed: 999}
	if again := s.Totals(); again["logs"].Removed != 1 {
		t.Errorf("Totals() is not a copy: got %+v after mutating the returned map", again["logs"])
	}
}
