// anchor-keep: close-then-delete order inside EndSpawn is a race-window fact (waiter must see a closed channel, not a vanished entry); no seam drives that interleaving deterministically.
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
	if _, ok := s.StartupFailure("k"); ok {
		t.Error("zero Store must not report a startup failure")
	}
	s.ClearStartupFailure("k")
	s.PruneStartupFailures(time.Now())
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

// A second BeginSpawn for an in-flight key gets the installed channel to wait
// on, and is told it does not own it.
func TestBeginSpawn_SecondCallerDoesNotOwn(t *testing.T) {
	var s Store
	a, owned := s.BeginSpawn("k")
	if a == nil || !owned {
		t.Fatalf("first BeginSpawn = %v, owned %v; want a fresh owned channel", a, owned)
	}
	if again, owned := s.BeginSpawn("k"); again != a || owned {
		t.Errorf("second BeginSpawn = owned %v, same channel %v; want the installed channel, not owned", owned, again == a)
	}
	if got, ok := s.SpawnInFlight("k"); !ok || got != a {
		t.Errorf("SpawnInFlight = %v,%v; want the installed channel", got, ok)
	}
	if b, owned := s.BeginSpawn("other"); b == a || !owned {
		t.Error("distinct keys must get distinct owned channels")
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

// Only the current marker is ended: a second end of the same channel, or an
// end with a channel that is not key's marker, closes nothing.
func TestEndSpawn_OnlyTheCurrentMarker(t *testing.T) {
	var s Store
	ch, _ := s.BeginSpawn("k")
	if !s.EndSpawn("k", ch) {
		t.Fatal("owner's EndSpawn reported no-op")
	}
	if s.EndSpawn("k", ch) {
		t.Error("second EndSpawn of the same channel reported a close")
	}
	next, _ := s.BeginSpawn("k")
	if s.EndSpawn("k", ch) {
		t.Error("EndSpawn with a stale channel retired the next spawn's marker")
	}
	select {
	case <-next:
		t.Fatal("a stale EndSpawn closed the next spawn's channel")
	default:
	}
	if got, ok := s.SpawnInFlight("k"); !ok || got != next {
		t.Error("the next spawn's marker is gone")
	}
	if s.EndSpawn("absent", make(chan struct{})) {
		t.Error("EndSpawn of an absent key reported a close")
	}
	// An absent key with a nil channel is a no-op too, not a close(nil) panic.
	if s.EndSpawn("absent", nil) {
		t.Error("EndSpawn(absent, nil) reported a close")
	}
}

func TestEndSpawn_ClosesOnceAndRemovesKey(t *testing.T) {
	var s Store
	ch, _ := s.BeginSpawn("k")
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
	next, _ := s.BeginSpawn("k")
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

func TestStartupFailure_NoteClearPrune(t *testing.T) {
	var s Store
	now := time.Unix(1_000_000, 0)
	old := StartupFailure{Streak: 3, At: now.Add(-time.Hour), Detail: "old"}
	recent := StartupFailure{Streak: 1, At: now, Detail: "recent"}
	s.NoteStartupFailure("old", old)
	s.NoteStartupFailure("recent", recent)
	s.NoteStartupFailure("cleared", recent)
	if got, ok := s.StartupFailure("old"); !ok || got != old {
		t.Fatalf("StartupFailure(old) = %+v,%v; want %+v", got, ok, old)
	}
	s.ClearStartupFailure("cleared")
	s.PruneStartupFailures(now.Add(-time.Minute))
	if _, ok := s.StartupFailure("cleared"); ok {
		t.Error("ClearStartupFailure must drop the run")
	}
	if _, ok := s.StartupFailure("old"); ok {
		t.Error("a run older than the cutoff must be pruned")
	}
	if got, ok := s.StartupFailure("recent"); !ok || got != recent {
		t.Errorf("StartupFailure(recent) = %+v,%v; want it kept", got, ok)
	}
}
