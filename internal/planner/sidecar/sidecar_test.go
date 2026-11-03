package sidecar

import (
	"os"
	"path/filepath"
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
	if !ok || m.V != Version || m.Purpose != PurposeTask || !m.DispatchedByPlanner() || !m.Created.Equal(now) || s.Purpose(key) != PurposeTask {
		t.Fatalf("reopened = %+v, %v", m, ok)
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
	if s.Purpose(key) != PurposeTask {
		t.Fatalf("purpose = %q", s.Purpose(key))
	}
	s.Sweep(func(string) bool { return false })
	if err := s.Put(Meta{Key: key, Purpose: PurposeTask}); err == nil {
		t.Error("Put overwrote a newer sidecar")
	}
	s.Remove(key)
	if got, err := os.ReadFile(file); err != nil || string(got) != string(data) {
		t.Errorf("newer sidecar changed: %q, %v", got, err)
	}
}
