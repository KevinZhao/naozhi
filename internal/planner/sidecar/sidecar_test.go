package sidecar

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
