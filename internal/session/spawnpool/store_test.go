package spawnpool

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestZeroValue_ReadsAreSafe(t *testing.T) {
	var s Store
	if s.PendingSpawns() != 0 || s.SpawningCount() != 0 {
		t.Errorf("zero Store: Pending=%d Spawning=%d", s.PendingSpawns(), s.SpawningCount())
	}
	if ch, ok := s.SpawnInFlight("k"); ok || ch != nil {
		t.Errorf("SpawnInFlight on zero Store = %v,%v; want nil,false", ch, ok)
	}
	if s.ShimStuck("k") || s.ConsumeShimStuck("k") {
		t.Error("zero Store must not report a stuck key")
	}
	s.ClearShimStuck("k")
	done := make(chan struct{})
	go func() { s.WaitRemoves(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitRemoves on zero Store must return immediately")
	}
}

func TestSpawnSlots_CountAcquireAndRelease(t *testing.T) {
	var s Store
	s.AcquireSpawnSlot()
	s.AcquireSpawnSlot()
	if s.PendingSpawns() != 2 {
		t.Fatalf("after two acquires Pending=%d, want 2", s.PendingSpawns())
	}
	s.ReleaseSpawnSlot()
	if s.PendingSpawns() != 1 {
		t.Fatalf("after one release Pending=%d, want 1", s.PendingSpawns())
	}
	s.ReleaseSpawnSlot()
	if s.PendingSpawns() != 0 {
		t.Fatalf("after balanced release Pending=%d, want 0", s.PendingSpawns())
	}
}

func TestBeginSpawn_ReusesInFlightChannel(t *testing.T) {
	var s Store
	a := s.BeginSpawn("k")
	if a == nil {
		t.Fatal("BeginSpawn returned nil channel")
	}
	if again := s.BeginSpawn("k"); again != a {
		t.Error("second BeginSpawn for an in-flight key must return the installed channel")
	}
	if got, ok := s.SpawnInFlight("k"); !ok || got != a {
		t.Errorf("SpawnInFlight = %v,%v; want the installed channel", got, ok)
	}
	if b := s.BeginSpawn("other"); b == a {
		t.Error("distinct keys must get distinct channels")
	}
	if s.SpawningCount() != 2 {
		t.Errorf("SpawningCount=%d, want 2", s.SpawningCount())
	}
	select {
	case <-a:
		t.Fatal("in-flight channel must stay open until EndSpawn")
	default:
	}
}

func TestEndSpawn_ClosesOnceAndRemovesKey(t *testing.T) {
	var s Store
	ch := s.BeginSpawn("k")
	s.EndSpawn("k", ch)
	select {
	case <-ch:
	default:
		t.Fatal("EndSpawn must close the done-channel")
	}
	if _, ok := s.SpawnInFlight("k"); ok {
		t.Error("key must be removed after EndSpawn")
	}
	if s.SpawningCount() != 0 {
		t.Errorf("SpawningCount=%d after EndSpawn, want 0", s.SpawningCount())
	}
	next := s.BeginSpawn("k")
	if next == ch {
		t.Fatal("BeginSpawn after EndSpawn must install a fresh channel, not the closed one")
	}
	select {
	case <-next:
		t.Fatal("fresh channel must be open")
	default:
	}
}

// Waiters hold the channel reference, so close-then-delete is the order that
// lets a caller racing in between observe a closed channel from the still
// present entry rather than a nil from a re-arrived spawn.
func TestEndSpawn_CloseBeforeDelete_Order(t *testing.T) {
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	body := string(src)
	closeIdx := strings.Index(body, "close(ch)")
	deleteIdx := strings.Index(body, "delete(s.spawning, key)")
	if closeIdx < 0 || deleteIdx < 0 {
		t.Fatalf("EndSpawn body changed: close=%d delete=%d", closeIdx, deleteIdx)
	}
	if closeIdx >= deleteIdx {
		t.Errorf("close(ch) at %d must precede delete(s.spawning, key) at %d", closeIdx, deleteIdx)
	}
}

func TestShimStuck_ConsumeClearsFlag(t *testing.T) {
	var s Store
	s.MarkShimStuck("a")
	s.MarkShimStuck("b")
	if !s.ShimStuck("a") {
		t.Fatal("ShimStuck(a) must be true after Mark")
	}
	if !s.ConsumeShimStuck("a") {
		t.Fatal("first Consume must report the flag")
	}
	if s.ShimStuck("a") || s.ConsumeShimStuck("a") {
		t.Error("flag must be gone after Consume")
	}
	if !s.ShimStuck("b") {
		t.Error("consuming a must not touch b")
	}
	s.ClearShimStuck("b")
	if s.ShimStuck("b") {
		t.Error("ClearShimStuck must drop the flag")
	}
	s.ClearShimStuck("missing")
}

func TestRemoveWG_WaitJoinsTrackedTeardowns(t *testing.T) {
	var s Store
	s.TrackRemove()
	release := make(chan struct{})
	go func() {
		<-release
		s.RemoveDone()
	}()
	waited := make(chan struct{})
	go func() { s.WaitRemoves(); close(waited) }()
	select {
	case <-waited:
		t.Fatal("WaitRemoves returned before RemoveDone")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitRemoves did not return after RemoveDone")
	}
}
