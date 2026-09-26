package sessiontable

import (
	"slices"
	"strings"
	"testing"
)

type sess struct{ id string }

func chatOf(key string) string {
	if i := strings.LastIndexByte(key, ':'); i >= 0 {
		return key[:i]
	}
	return key
}

func hashOf(key string) string { return "h(" + key + ")" }

func newTable() *Table[*sess] { return New[*sess](chatOf, hashOf) }

func idOf(s *sess) string { return s.id }

func consistent(t *testing.T, tab *Table[*sess], after string) {
	t.Helper()
	if p := tab.Check(idOf); len(p) > 0 {
		t.Fatalf("after %s:\n  %s", after, strings.Join(p, "\n  "))
	}
}

func TestTable_PutDeleteKeepIndicesInStep(t *testing.T) {
	tab := newTable()
	a, b := &sess{}, &sess{}
	tab.Put("feishu:chat1:a", a)
	tab.Put("feishu:chat1:b", b)
	consistent(t, tab, "two puts")
	if got, ok := tab.Lookup("feishu:chat1:a"); !ok || got != a {
		t.Fatal("Lookup does not find a put session")
	}
	if k, ok := tab.KeyForHash(hashOf("feishu:chat1:b")); !ok || k != "feishu:chat1:b" {
		t.Errorf("KeyForHash = (%q,%v)", k, ok)
	}
	keys := tab.KeysOfChat("feishu:chat1")
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"feishu:chat1:a", "feishu:chat1:b"}) {
		t.Errorf("KeysOfChat = %v", keys)
	}

	tab.Delete("feishu:chat1:a")
	consistent(t, tab, "delete a")
	if tab.Len() != 1 || !tab.ChatHasSessions("feishu:chat1") {
		t.Fatalf("after deleting one of two: len=%d chatHas=%v", tab.Len(), tab.ChatHasSessions("feishu:chat1"))
	}
	tab.Delete("feishu:chat1:b")
	consistent(t, tab, "delete b")
	if tab.ChatHasSessions("feishu:chat1") || len(tab.byChat) != 0 {
		t.Error("the emptied chat's set was kept")
	}
	if _, ok := tab.KeyForHash(hashOf("feishu:chat1:b")); ok {
		t.Error("a deleted key still resolves by hash")
	}
	tab.Delete("feishu:chat1:missing") // no-op
	consistent(t, tab, "deleting a missing key")
}

// TestTable_DeleteKeepsACollidingHash: two keys hashing alike must not let one
// key's delete remove the other's entry.
func TestTable_DeleteKeepsACollidingHash(t *testing.T) {
	tab := New[*sess](chatOf, func(string) string { return "same" })
	tab.Put("x:a", &sess{})
	tab.Put("x:b", &sess{}) // takes over the hash
	tab.Delete("x:a")
	if k, ok := tab.KeyForHash("same"); !ok || k != "x:b" {
		t.Errorf("deleting x:a removed x:b's hash entry: (%q,%v)", k, ok)
	}
}

func TestTable_IDIndex(t *testing.T) {
	tab := newTable()
	s := &sess{id: "sid-1"}
	tab.Put("c:a", s)
	if p := tab.Check(idOf); len(p) != 1 || !strings.Contains(p[0], `reports id "sid-1" with no idToKey entry`) {
		t.Fatalf("an unindexed ID should be the one problem, got %v", p)
	}
	tab.SetID("sid-1", "c:a")
	consistent(t, tab, "SetID")
	if k, ok := tab.KeyForID("sid-1"); !ok || k != "c:a" {
		t.Errorf("KeyForID = (%q,%v)", k, ok)
	}
	tab.SetID("", "c:a") // ignored
	if _, ok := tab.KeyForID(""); ok {
		t.Error("an empty ID was indexed")
	}

	tab.ClearIDIfOwnedBy("sid-1", "c:other")
	if _, ok := tab.KeyForID("sid-1"); !ok {
		t.Error("ClearIDIfOwnedBy removed a mapping owned by another key")
	}
	tab.ClearIDIfOwnedBy("sid-1", "c:a")
	if _, ok := tab.KeyForID("sid-1"); ok {
		t.Error("ClearIDIfOwnedBy kept the owner's mapping")
	}
	tab.SetID("sid-1", "c:a")
	tab.ClearID("sid-1")
	if _, ok := tab.KeyForID("sid-1"); ok {
		t.Error("ClearID kept the mapping")
	}
}

func TestTable_CheckReportsEachDirection(t *testing.T) {
	tab := newTable()
	tab.Put("c:a", &sess{})
	tab.SetID("sid-gone", "c:gone")
	tab.byChat["c"]["c:ghost"] = struct{}{}
	tab.keyhash["stray"] = "c:ghost"
	tab.byChat["empty"] = map[string]struct{}{}
	p := strings.Join(tab.Check(idOf), "\n")
	for _, want := range []string{`idToKey["sid-gone"] holds dead session`, `byChat["c"] holds dead session "c:ghost"`, `keyhash["stray"] holds dead session`, `byChat["empty"] is an empty set`} {
		if !strings.Contains(p, want) {
			t.Errorf("Check missed %q:\n%s", want, p)
		}
	}
}

func TestTable_CountersAndFlags(t *testing.T) {
	tab := newTable()
	if tab.AddActive(2) != 2 || tab.Active() != 2 {
		t.Fatal("AddActive")
	}
	tab.SetActive(5)
	if tab.Active() != 5 {
		t.Fatal("SetActive")
	}
	g := tab.Gen()
	tab.BumpGen()
	if tab.Gen() != g+1 || tab.Dirty() {
		t.Fatal("BumpGen must advance the generation without marking dirty")
	}
	tab.MarkChanged()
	if tab.Gen() != g+2 || !tab.Dirty() {
		t.Fatal("MarkChanged must mark dirty and advance")
	}
	tab.SetDirty(false)
	if tab.Dirty() || tab.Gen() != g+2 {
		t.Fatal("SetDirty must not touch the generation")
	}
}

func TestTable_AllStopsWhenAsked(t *testing.T) {
	tab := newTable()
	for _, k := range []string{"c:a", "c:b", "c:c"} {
		tab.Put(k, &sess{})
	}
	n := 0
	for range tab.All() {
		n++
		break
	}
	if n != 1 {
		t.Errorf("All yielded %d after break", n)
	}
	seen := 0
	for range tab.All() {
		seen++
	}
	if seen != 3 {
		t.Errorf("All yielded %d of 3", seen)
	}
}

// TestTable_WaitWakesOnBroadcast: Wait releases the lock while parked and
// holds it again when it returns.
func TestTable_WaitWakesOnBroadcast(t *testing.T) {
	tab := newTable()
	ready := false
	done := make(chan struct{})
	tab.Lock()
	go func() {
		tab.Lock()
		ready = true
		tab.Broadcast()
		tab.Unlock()
		close(done)
	}()
	for !ready {
		tab.Wait() // must release the lock, or the goroutine above deadlocks
	}
	if tab.TryLock() {
		t.Error("Wait returned without the lock held")
	}
	tab.Unlock()
	<-done
}
