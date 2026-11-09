package sidecar

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// A sidecar survives a reopen, is marked retired once its session is gone,
// comes back with it, and is removed Retention after it retired.
func TestSidecarRetention(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	key := TaskKey()
	if !sessionkey.IsTaskKey(key) || key == TaskKey() {
		t.Fatalf("TaskKey = %q", key)
	}
	s := Open(dir, clock)
	if err := s.Put(Meta{Key: key, Purpose: PurposeTask, Parent: sessionkey.PlannerMainKey, Title: "写 hello"}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, fileName(key))
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("sidecar file: %v %v", fi, err)
	}

	s = Open(dir, clock)
	m, ok := s.Get(key)
	if !ok || m.V != Version || m.Purpose != PurposeTask || !m.DispatchedByPlanner() || !m.Created.Equal(now) {
		t.Fatalf("reopened = %+v, %v", m, ok)
	}
	if p, title := s.Purpose(key); p != PurposeTask || title != "写 hello" {
		t.Fatalf("Purpose = %q, %q", p, title)
	}

	alive := true
	live := func(string) bool { return alive }
	steps := []struct {
		after       time.Duration
		alive       bool
		wantRetired time.Time
		wantGone    bool
	}{
		{0, true, time.Time{}, false},
		{time.Hour, false, now.Add(time.Hour), false},
		{2 * time.Hour, true, time.Time{}, false}, // came back
		{3 * time.Hour, false, now.Add(3 * time.Hour), false},
		{3*time.Hour + Retention, false, now.Add(3 * time.Hour), false},
		{3*time.Hour + Retention + time.Minute, false, time.Time{}, true},
	}
	start := now
	for i, st := range steps {
		now, alive = start.Add(st.after), st.alive
		s.Sweep(live)
		m, ok := Open(dir, clock).Get(key)
		_, err := os.Stat(file)
		if st.wantGone {
			if ok || !os.IsNotExist(err) {
				t.Errorf("step %d: sidecar kept past retention (%v, %v)", i, ok, err)
			}
			continue
		}
		if !ok || !m.Retired.Equal(st.wantRetired) {
			t.Errorf("step %d: retired = %v (ok %v), want %v", i, m.Retired, ok, st.wantRetired)
		}
	}
}

// A sidecar a newer naozhi wrote is read but never rewritten or removed, so
// the fields this build does not know survive a downgrade.
func TestSidecarNewerVersionKept(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	key := TaskKey()
	file := filepath.Join(dir, fileName(key))
	data := []byte(`{"v":2,"key":"` + key + `","purpose":"task","contract":{"goal":"x"},"retired":"2026-01-01T00:00:00Z"}`)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s := Open(dir, func() time.Time { return now })
	if p, _ := s.Purpose(key); p != PurposeTask {
		t.Fatalf("purpose = %q", p)
	}
	s.Sweep(func(string) bool { return false })
	if err := s.Put(Meta{Key: key, Purpose: PurposeTask}); !errors.Is(err, ErrNewer) {
		t.Errorf("Put on a newer sidecar = %v, want ErrNewer", err)
	}
	if ok, err := s.Update(key, func(*Meta) bool { return true }); !ok || !errors.Is(err, ErrNewer) {
		t.Errorf("Update on a newer sidecar = %v, %v; want true, ErrNewer", ok, err)
	}
	s.Remove(key)
	if got, err := os.ReadFile(file); err != nil || string(got) != string(data) {
		t.Errorf("newer sidecar changed: %q, %v", got, err)
	}
}

// Ensure creates in the same step it checks: another writer's Upsert that
// lands between a lookup and a create is not replaced by an empty sidecar.
func TestEnsureKeepsAConcurrentUpsert(t *testing.T) {
	s := Open("", time.Now)
	at := time.Unix(100, 0)
	lost := 0
	const n = 20000
	for i := range n {
		key := fmt.Sprintf("dashboard:direct:u%d:general", i)
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if err := s.Ensure(key); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if err := s.Upsert(key, func(m *Meta) bool { m.ResultAt = at; return true }); err != nil {
				t.Error(err)
			}
		}()
		close(start)
		wg.Wait()
		if m, ok := s.Get(key); !ok || !m.ResultAt.Equal(at) {
			lost++
		}
	}
	if lost > 0 {
		t.Errorf("%d of %d sidecars lost the ResultAt an Upsert wrote beside Ensure", lost, n)
	}
}

// Ensure writes a sidecar only for a key without one: it leaves one with
// fields, and one a newer naozhi wrote, as they are.
func TestEnsure(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	s := Open(dir, func() time.Time { return now })
	const key = "dashboard:direct:u:general"
	if err := s.Ensure(key); err != nil {
		t.Fatal(err)
	}
	m, ok := s.Get(key)
	if !ok || m.Purpose != "" || !m.Created.Equal(now) || m.V != Version {
		t.Fatalf("Ensure made %+v (%v), want an empty sidecar created now", m, ok)
	}
	if err := s.Upsert(key, func(m *Meta) bool { m.SeenAt = now; return true }); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := s.Ensure(key); err != nil {
		t.Fatal(err)
	}
	if m, _ = Open(dir, func() time.Time { return now }).Get(key); !m.SeenAt.Equal(now.Add(-time.Hour)) || !m.Created.Equal(now.Add(-time.Hour)) {
		t.Errorf("after a second Ensure, on disk %+v, want SeenAt and Created kept", m)
	}

	newer := TaskKey()
	file := filepath.Join(dir, fileName(newer))
	data := []byte(`{"v":99,"key":"` + newer + `","purpose":"task"}`)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s = Open(dir, func() time.Time { return now })
	if err := s.Ensure(newer); err != nil {
		t.Errorf("Ensure on a newer sidecar = %v, want nil: it has one", err)
	}
	if got, _ := os.ReadFile(file); string(got) != string(data) {
		t.Errorf("Ensure rewrote a newer sidecar: %s", got)
	}
	if err := s.Ensure(""); err == nil {
		t.Error("Ensure(\"\") = nil, want an error")
	}
}

// ReportBlock is a field added without a version bump: kept across a
// reopen when set, absent from the file when not, so a sidecar of before
// reads as a session reporting by SendMessage.
func TestReportBlockField(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir, nil)
	block, old := TaskKey(), TaskKey()
	for key, on := range map[string]bool{block: true, old: false} {
		if err := s.Put(Meta{Key: key, Purpose: PurposeTask, ReportBlock: on}); err != nil {
			t.Fatal(err)
		}
	}
	re := Open(dir, nil)
	if m, _ := re.Get(block); !m.ReportBlock {
		t.Errorf("reopened %+v, want ReportBlock kept", m)
	}
	if m, _ := re.Get(old); m.ReportBlock {
		t.Errorf("reopened %+v, want no ReportBlock", m)
	}
	data, err := os.ReadFile(filepath.Join(dir, fileName(old)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "report_block") {
		t.Errorf("unset ReportBlock written: %s", data)
	}
}

// BaselineSeen sets seen_at once per directory on the sidecars with none,
// leaves a set one and a newer naozhi's file alone, and does nothing in
// memory or on a second call.
func TestBaselineSeen(t *testing.T) {
	t0 := time.Date(2026, 11, 9, 3, 0, 0, 0, time.UTC)
	seen := t0.Add(-time.Hour)
	dir := t.TempDir()
	s := Open(dir, func() time.Time { return t0 })
	for _, m := range []Meta{
		{Key: "task:aaaaaaaaaaaa", Purpose: PurposeTask},
		{Key: "task:bbbbbbbbbbbb", Purpose: PurposeTask, SeenAt: seen},
	} {
		if err := s.Put(m); err != nil {
			t.Fatal(err)
		}
	}
	newerKey := "task:cccccccccccc"
	newer := []byte(`{"v":99,"key":"` + newerKey + `","purpose":"task","created":"2026-11-04T08:00:00Z"}`)
	newerPath := filepath.Join(dir, fileName(newerKey))
	if err := os.WriteFile(newerPath, newer, 0o600); err != nil {
		t.Fatal(err)
	}
	later := t0.Add(time.Hour)
	s = Open(dir, func() time.Time { return later })
	if n, err := s.BaselineSeen(); n != 1 || err != nil {
		t.Fatalf("BaselineSeen = %d, %v; want 1, nil", n, err)
	}
	if m, _ := s.Get("task:aaaaaaaaaaaa"); !m.SeenAt.Equal(later) {
		t.Errorf("unseen sidecar seen_at = %v, want %v", m.SeenAt, later)
	}
	if m, _ := s.Get("task:bbbbbbbbbbbb"); !m.SeenAt.Equal(seen) {
		t.Errorf("seen sidecar seen_at = %v, want %v", m.SeenAt, seen)
	}
	if got, _ := os.ReadFile(newerPath); string(got) != string(newer) {
		t.Errorf("newer sidecar rewritten: %s", got)
	}
	// On disk too, and the marker keeps a second start from it.
	s = Open(dir, func() time.Time { return later.Add(time.Hour) })
	if m, _ := s.Get("task:aaaaaaaaaaaa"); !m.SeenAt.Equal(later) {
		t.Errorf("reloaded seen_at = %v, want %v", m.SeenAt, later)
	}
	if err := s.Put(Meta{Key: "task:dddddddddddd", Purpose: PurposeTask}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.BaselineSeen(); n != 0 || err != nil {
		t.Fatalf("second BaselineSeen = %d, %v; want 0, nil", n, err)
	}
	if m, _ := s.Get("task:dddddddddddd"); !m.SeenAt.IsZero() {
		t.Errorf("sidecar written after the baseline got seen_at %v", m.SeenAt)
	}
	if n, err := Open("", nil).BaselineSeen(); n != 0 || err != nil {
		t.Errorf("in-memory BaselineSeen = %d, %v", n, err)
	}
}
