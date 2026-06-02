package wireup

import (
	"reflect"
	"sync"
	"testing"
)

// TestRegistry_RegisterGet pins the basic register→lookup contract that the
// unified R244-ARCH-4 (#1058) idiom must provide.
func TestRegistry_RegisterGet(t *testing.T) {
	t.Parallel()

	r := NewRegistry[int]("backend")
	r.Register("claude", 1)
	r.Register("kiro", 2)

	if v, ok := r.Get("claude"); !ok || v != 1 {
		t.Fatalf("Get(claude) = (%d,%v), want (1,true)", v, ok)
	}
	if _, ok := r.Get("missing"); ok {
		t.Fatal("Get(missing) reported present")
	}
	if r.Len() != 2 {
		t.Fatalf("Len = %d, want 2", r.Len())
	}
}

// TestRegistry_Names_Sorted ensures the audit listing is deterministic —
// the unified registry's value over bespoke wiring is an inspectable
// "what is wired" view, which a nondeterministic order would defeat.
func TestRegistry_Names_Sorted(t *testing.T) {
	t.Parallel()

	r := NewRegistry[struct{}]("platform")
	for _, n := range []string{"feishu", "dingtalk", "slack"} {
		r.Register(n, struct{}{})
	}
	got := r.Names()
	want := []string{"dingtalk", "feishu", "slack"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Names = %v, want sorted %v", got, want)
	}
}

// TestRegistry_DuplicatePanics pins the fail-loud-at-boot contract: a
// duplicate registration must panic (matching cli.RegisterHistoryFactory /
// backend.Register), not silently shadow the earlier entry.
func TestRegistry_DuplicatePanics(t *testing.T) {
	t.Parallel()

	r := NewRegistry[int]("cron-daemon")
	r.Register("titler", 1)

	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration did not panic")
		}
	}()
	r.Register("titler", 2)
}

// TestRegistry_EmptyNamePanics ensures an unaddressable empty-name entry is
// rejected at registration time.
func TestRegistry_EmptyNamePanics(t *testing.T) {
	t.Parallel()

	r := NewRegistry[int]("backend")
	defer func() {
		if recover() == nil {
			t.Fatal("empty-name registration did not panic")
		}
	}()
	r.Register("", 1)
}

// TestRegistry_ConcurrentRegister exercises the mutex under the race
// detector — boot-time init() functions across packages may register
// concurrently, so the registry must be safe without external locking.
func TestRegistry_ConcurrentRegister(t *testing.T) {
	t.Parallel()

	r := NewRegistry[int]("backend")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.Register(string(rune('a'+i%26))+string(rune('0'+i/26)), i)
			_ = r.Names()
			_, _ = r.Get("a0")
		}(i)
	}
	wg.Wait()
	if r.Len() != 50 {
		t.Fatalf("Len = %d after 50 concurrent registers, want 50", r.Len())
	}
}
