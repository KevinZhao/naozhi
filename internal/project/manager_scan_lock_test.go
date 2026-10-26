package project

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pauseFirstScanDisk makes m's next scanDisk stop at its end, after all of
// its disk reads, until release is closed; entered is closed once it gets
// there. calls counts every scanDisk pass, so a re-read under m.mu shows up.
func pauseFirstScanDisk(t *testing.T, m *Manager) (entered chan struct{}, release func(), calls *atomic.Int32) {
	t.Helper()
	entered = make(chan struct{})
	gate := make(chan struct{})
	release = sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	calls = new(atomic.Int32)
	m.scanDiskHook = func() {
		if calls.Add(1) == 1 {
			close(entered)
			<-gate
		}
	}
	return entered, release, calls
}

// returnsPromptly fails the test when fn is still blocked after 10s.
func returnsPromptly(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s blocked while Scan's disk read was in flight", what)
	}
}

func startScan(m *Manager) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- m.Scan() }()
	return errc
}

// A writer that persists while Scan reads disk lock-free must survive the
// swap: Scan sees mutGen move and re-reads under the lock, where the
// writer's project.yaml is already on disk.
func TestScan_WriterDuringDiskReadSurvivesSwap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		write func(m *Manager) error
		check func(m *Manager) error
	}{
		{
			name:  "BindChat",
			write: func(m *Manager) error { return m.BindChat("proj", "feishu", "group", "new") },
			check: func(m *Manager) error {
				if m.ProjectForChat("feishu", "group", "new") == nil {
					return fmt.Errorf("binding added during the disk read was dropped")
				}
				return nil
			},
		},
		{
			name:  "UnbindAllChat",
			write: func(m *Manager) error { return m.UnbindAllChat("feishu", "group", "old") },
			check: func(m *Manager) error {
				if m.ProjectForChat("feishu", "group", "old") != nil {
					return fmt.Errorf("binding removed during the disk read came back")
				}
				return nil
			},
		},
		{
			name:  "SetFavorite",
			write: func(m *Manager) error { return m.SetFavorite("proj", true) },
			check: func(m *Manager) error {
				if !m.Get("proj").Config.Favorite {
					return fmt.Errorf("favorite set during the disk read was dropped")
				}
				return nil
			},
		},
		{
			name:  "UpdateConfig",
			write: func(m *Manager) error { return m.UpdateConfig("proj", ProjectConfig{PlannerModel: "sonnet"}) },
			check: func(m *Manager) error {
				if got := m.Get("proj").Config.PlannerModel; got != "sonnet" {
					return fmt.Errorf("planner_model = %q, want the update's sonnet", got)
				}
				return nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			makeProjectDir(t, root, "proj", &ProjectConfig{
				CreatedAt:    1,
				ChatBindings: []ChatBinding{{Platform: "feishu", ChatType: "group", ChatID: "old"}},
			})
			m, _ := NewManager(root, PlannerDefaults{})
			if err := m.Scan(); err != nil {
				t.Fatal(err)
			}
			entered, release, calls := pauseFirstScanDisk(t, m)
			errc := startScan(m)
			<-entered

			returnsPromptly(t, tc.name, func() {
				if err := tc.write(m); err != nil {
					t.Errorf("%s: %v", tc.name, err)
				}
			})
			release()
			if err := <-errc; err != nil {
				t.Fatal(err)
			}
			if err := tc.check(m); err != nil {
				t.Error(err)
			}
			if n := calls.Load(); n != 2 {
				t.Errorf("scanDisk ran %d times, want 2 (lock-free read, then the re-read)", n)
			}
		})
	}
}

// Scans from the periodic loop and from callers overlap with each other and
// with writers; scanMu keeps the index and the swap consistent (-race).
func TestScan_ConcurrentScansAndWriters(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d"} {
		makeProjectDir(t, root, name, nil)
	}
	indexPath := filepath.Join(t.TempDir(), "projects-index.json")
	m, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(indexPath))
	if err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	want := projectNames(m.All())

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if err := m.Scan(); err != nil {
					t.Errorf("Scan: %v", err)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 20 {
			if err := m.SetFavorite("b", i%2 == 0); err != nil {
				t.Errorf("SetFavorite: %v", err)
			}
		}
	}()
	wg.Wait()

	if got := projectNames(m.All()); got != want {
		t.Errorf("order after concurrent scans = %s, want %s", got, want)
	}
	if m.Get("b").Config.Favorite {
		t.Error("favorite = true, want the last write's false")
	}
	fresh, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(indexPath))
	if err := fresh.Scan(); err != nil {
		t.Fatal(err)
	}
	if got := projectNames(fresh.All()); got != want {
		t.Errorf("order from the persisted index = %s, want %s", got, want)
	}
}
