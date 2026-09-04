package knownids

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

const interval = 5 * time.Minute

func TestZeroValue_Usable(t *testing.T) {
	var s Store
	if s.Has("x") || s.Len() != 0 || s.Dirty() || s.Gen() != 0 || !s.SavedAt().IsZero() {
		t.Fatal("zero-value store must read as empty/clean")
	}
	if got := s.SortedSnapshot(); len(got) != 0 {
		t.Errorf("SortedSnapshot on empty = %v, want empty", got)
	}
	data, err := s.MarshalSnapshot()
	if err != nil || string(data) != "[]" {
		t.Errorf("MarshalSnapshot on empty = %q, %v; want \"[]\"", data, err)
	}
	if _, _, due, err := s.ClaimSave(time.Now(), interval); due || err != nil {
		t.Errorf("clean store must not be due (due=%v err=%v)", due, err)
	}
	if !s.MarkSavedIfUnchanged(0) {
		t.Error("MarkSavedIfUnchanged(0) on untouched store must succeed")
	}
	s.ResetSaveThrottle()
	if !s.Track("a") {
		t.Fatal("first Track on zero value must add")
	}
	if !s.Has("a") || s.Len() != 1 || !s.Dirty() || s.Gen() != 1 {
		t.Error("state after first Track wrong")
	}
}

func TestTrack_DedupesAndIgnoresEmpty(t *testing.T) {
	var s Store
	if s.Track("") {
		t.Error("empty id must not be tracked")
	}
	if s.Len() != 0 || s.Dirty() {
		t.Error("empty id must not touch state")
	}
	if !s.Track("dup") || s.Track("dup") || s.Track("dup") {
		t.Error("only the first Track of an id may report added")
	}
	if s.Len() != 1 || s.Gen() != 1 {
		t.Errorf("Len=%d Gen=%d, want 1/1 after duplicates", s.Len(), s.Gen())
	}
	if got := len(s.order) - s.orderHead; got != 1 {
		t.Errorf("live window = %d, want 1", got)
	}
}

func TestTrack_CapsAtMaxKnownIDs_FIFO(t *testing.T) {
	var s Store
	total := MaxKnownIDs + 5
	for i := 0; i < total; i++ {
		s.Track(fmt.Sprintf("sess-%07d", i))
	}
	if got := s.Len(); got != MaxKnownIDs {
		t.Errorf("Len = %d, want capped at %d", got, MaxKnownIDs)
	}
	if got := len(s.order) - s.orderHead; got != MaxKnownIDs {
		t.Errorf("live window = %d, want %d", got, MaxKnownIDs)
	}
	for i := 0; i < 5; i++ {
		if id := fmt.Sprintf("sess-%07d", i); s.Has(id) {
			t.Errorf("oldest ID %q was not evicted", id)
		}
	}
	if !s.Has(fmt.Sprintf("sess-%07d", 5)) {
		t.Error("first surviving ID missing")
	}
	want := fmt.Sprintf("sess-%07d", total-1)
	if got := s.order[len(s.order)-1]; got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}
	if uint64(total) != s.Gen() {
		t.Errorf("Gen = %d, want %d (one bump per add)", s.Gen(), total)
	}
}

func TestTrack_HeadCompactionBoundsMemory(t *testing.T) {
	var s Store
	total := MaxKnownIDs + 20000
	for i := 0; i < total; i++ {
		s.Track(fmt.Sprintf("sess-%07d", i))
	}
	if got := s.Len(); got != MaxKnownIDs {
		t.Errorf("Len = %d, want %d", got, MaxKnownIDs)
	}
	if got := len(s.order) - s.orderHead; got != MaxKnownIDs {
		t.Errorf("live window = %d, want %d", got, MaxKnownIDs)
	}
	if got := cap(s.order); got >= 2*MaxKnownIDs {
		t.Errorf("cap(order) = %d, want < %d (compaction not releasing dead prefix)", got, 2*MaxKnownIDs)
	}
	want := fmt.Sprintf("sess-%07d", total-1)
	if got := s.order[len(s.order)-1]; got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}
	// Live window must mirror the map exactly.
	for _, id := range s.order[s.orderHead:] {
		if !s.ids[id] {
			t.Fatalf("order entry %q not in ids", id)
		}
	}
}

func TestSeed_NoDirtyNoGen_SkipsDuplicates(t *testing.T) {
	var s Store
	s.Seed(nil)
	if s.Len() != 0 || s.Dirty() {
		t.Error("Seed(nil) must be a no-op")
	}
	s.Track("live")
	gen := s.Gen()
	s.MarkSavedIfUnchanged(gen)
	s.Seed(map[string]bool{"a": true, "b": true, "live": true})
	if s.Len() != 3 || !s.Has("a") || !s.Has("b") {
		t.Errorf("Seed did not install entries: Len=%d", s.Len())
	}
	if s.Dirty() || s.Gen() != gen {
		t.Errorf("Seed must not dirty or bump gen (dirty=%v gen=%d want %d)", s.Dirty(), s.Gen(), gen)
	}
	if got := len(s.order) - s.orderHead; got != 3 {
		t.Errorf("order must hold each id once, live window = %d", got)
	}
	// Seeded entries participate in FIFO eviction.
	for i := 0; s.Len() < MaxKnownIDs; i++ {
		s.Track(fmt.Sprintf("fill-%d", i))
	}
	s.Track("overflow")
	if s.Has("live") {
		t.Error("oldest (first-tracked) entry must be evicted first")
	}
}

func TestClaimSave_ThrottleAndDirtySemantics(t *testing.T) {
	var s Store
	now := time.Now()
	s.Track("b")
	s.Track("a")

	data, gen, due, err := s.ClaimSave(now, interval)
	if err != nil || !due {
		t.Fatalf("dirty store with open throttle must be due (due=%v err=%v)", due, err)
	}
	if string(data) != `["a","b"]` || gen != s.Gen() {
		t.Errorf("ClaimSave = %q gen=%d, want sorted JSON at gen %d", data, gen, s.Gen())
	}
	if !s.SavedAt().Equal(now) {
		t.Errorf("SavedAt = %v, want stamped %v", s.SavedAt(), now)
	}
	if !s.Dirty() {
		t.Error("ClaimSave must not clear dirty; the write may still fail")
	}

	// Within the interval the window is closed even though still dirty.
	if _, _, due, _ := s.ClaimSave(now.Add(interval-time.Second), interval); due {
		t.Error("second claim inside the interval must not be due")
	}
	if _, _, due, _ := s.ClaimSave(now.Add(interval), interval); !due {
		t.Error("claim exactly at the interval must be due")
	}
	// Success path clears dirty; the next claim is not due regardless of time.
	if !s.MarkSavedIfUnchanged(gen) {
		t.Error("MarkSavedIfUnchanged with matching gen must succeed")
	}
	if s.Dirty() {
		t.Error("dirty must be cleared after MarkSavedIfUnchanged")
	}
	if _, _, due, _ := s.ClaimSave(now.Add(time.Hour), interval); due {
		t.Error("clean store must never be due")
	}
	// A failed write reopens the throttle without touching dirty.
	s.Track("c")
	if _, _, due, _ := s.ClaimSave(now.Add(2*time.Hour), interval); !due {
		t.Fatal("dirty again → due")
	}
	if _, _, due, _ := s.ClaimSave(now.Add(2*time.Hour), interval); due {
		t.Fatal("same instant re-claim must be throttled")
	}
	s.ResetSaveThrottle()
	if !s.SavedAt().IsZero() || !s.Dirty() {
		t.Error("ResetSaveThrottle must zero savedAt and keep dirty")
	}
	if _, _, due, _ := s.ClaimSave(now.Add(2*time.Hour), interval); !due {
		t.Error("after ResetSaveThrottle the claim must be due again")
	}
}

func TestClaimSave_MarshalErrorReleasesClaim(t *testing.T) {
	boom := errors.New("boom")
	orig := marshalJSON
	marshalJSON = func(any) ([]byte, error) { return nil, boom }
	t.Cleanup(func() { marshalJSON = orig })

	var s Store
	s.Track("x")
	now := time.Now()
	data, gen, due, err := s.ClaimSave(now, interval)
	if !errors.Is(err, boom) || err.Error() != "marshal known IDs: boom" {
		t.Fatalf("err = %v, want wrapped boom", err)
	}
	if data != nil || gen != 0 || due {
		t.Errorf("error path must return zero results, got %q %d %v", data, gen, due)
	}
	if !s.SavedAt().IsZero() {
		t.Error("marshal error must release the claim (savedAt reset)")
	}
	if !s.Dirty() {
		t.Error("marshal error must leave dirty set")
	}
	if _, err := s.MarshalSnapshot(); !errors.Is(err, boom) {
		t.Errorf("MarshalSnapshot err = %v, want boom", err)
	}
	if s.marshaledCache != nil {
		t.Error("failed marshal must not populate the cache")
	}

	marshalJSON = orig
	if _, _, due, err := s.ClaimSave(now, interval); !due || err != nil {
		t.Errorf("next tick after a marshal error must retry (due=%v err=%v)", due, err)
	}
}

func TestMarkSavedIfUnchanged_ConcurrentTrackKeepsDirty(t *testing.T) {
	var s Store
	s.Track("a")
	_, gen, due, err := s.ClaimSave(time.Now(), interval)
	if !due || err != nil {
		t.Fatal("expected due")
	}
	// An add + evict pair keeps Len identical, so length is not a valid
	// unchanged signal — gen is.
	s.Track("b")
	if s.MarkSavedIfUnchanged(gen) {
		t.Error("gen moved on; MarkSavedIfUnchanged must refuse")
	}
	if !s.Dirty() {
		t.Error("dirty must stay set so the next tick re-persists")
	}
	if !s.MarkSavedIfUnchanged(s.Gen()) || s.Dirty() {
		t.Error("current gen must clear dirty")
	}
}

func TestSnapshots_MemoisedByGen_NoAliasing(t *testing.T) {
	var s Store
	for _, id := range []string{"ccc", "aaa", "bbb"} {
		s.Track(id)
	}
	want := []string{"aaa", "bbb", "ccc"}

	got1 := s.SortedSnapshot()
	if !slices.Equal(got1, want) {
		t.Fatalf("SortedSnapshot = %v, want %v", got1, want)
	}
	if s.sortedGen != s.gen {
		t.Fatalf("sortedGen %d != gen %d", s.sortedGen, s.gen)
	}
	sortedPtr := &s.sortedCache[0]
	if &got1[0] == sortedPtr {
		t.Error("SortedSnapshot aliases the cache")
	}

	m1, err := s.MarshalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	var decoded []string
	if err := json.Unmarshal(m1, &decoded); err != nil || !slices.Equal(decoded, want) {
		t.Fatalf("MarshalSnapshot = %q (%v), want %v", m1, err, want)
	}
	if s.marshaledGen != s.gen || s.marshaledCache == nil {
		t.Fatal("marshaled cache not tagged with current gen")
	}
	marshaledPtr := &s.marshaledCache[0]
	if &m1[0] == marshaledPtr {
		t.Error("MarshalSnapshot aliases the cache")
	}

	// No mutation: both caches must be reused (same backing arrays).
	got2 := s.SortedSnapshot()
	m2, _ := s.MarshalSnapshot()
	if &s.sortedCache[0] != sortedPtr {
		t.Error("sorted cache rebuilt despite no mutation")
	}
	if &s.marshaledCache[0] != marshaledPtr {
		t.Error("marshaled cache rebuilt despite no mutation")
	}
	if !slices.Equal(got2, want) || string(m2) != string(m1) {
		t.Error("memoised snapshots differ from first")
	}
	// Mutating a returned snapshot must not leak into the cache.
	got2[0] = "zzz"
	m2[1] = 'x'
	if s.sortedCache[0] != "aaa" || s.marshaledCache[1] != m1[1] {
		t.Error("caller mutation of a snapshot reached the cache")
	}

	// A new ID bumps gen: both caches rebuild in sorted order.
	s.Track("aab")
	want3 := []string{"aaa", "aab", "bbb", "ccc"}
	got3 := s.SortedSnapshot()
	m3, _ := s.MarshalSnapshot()
	if !slices.Equal(got3, want3) {
		t.Errorf("post-mutation SortedSnapshot = %v, want %v", got3, want3)
	}
	var decoded3 []string
	if err := json.Unmarshal(m3, &decoded3); err != nil || !slices.Equal(decoded3, want3) {
		t.Errorf("post-mutation MarshalSnapshot = %q, want %v", m3, want3)
	}
	if s.sortedGen != s.gen || s.marshaledGen != s.gen {
		t.Error("caches not re-tagged after mutation")
	}
	// ClaimSave shares the same memo: identical bytes, no rebuild.
	marshaledPtr3 := &s.marshaledCache[0]
	data, _, due, _ := s.ClaimSave(time.Now(), interval)
	if !due || string(data) != string(m3) {
		t.Errorf("ClaimSave bytes = %q, want memoised %q", data, m3)
	}
	if &s.marshaledCache[0] != marshaledPtr3 {
		t.Error("ClaimSave rebuilt the marshaled cache despite no mutation")
	}
}

func TestMarshalSnapshot_DeterministicAcrossCalls(t *testing.T) {
	var a, b Store
	for _, id := range []string{"d", "a", "c", "b"} {
		a.Track(id)
	}
	for _, id := range []string{"b", "c", "a", "d"} {
		b.Track(id)
	}
	ma, _ := a.MarshalSnapshot()
	mb, _ := b.MarshalSnapshot()
	if string(ma) != string(mb) || string(ma) != `["a","b","c","d"]` {
		t.Errorf("same logical set must marshal identically: %q vs %q", ma, mb)
	}
}

// TestConcurrent_TrackClaimSnapshot_RaceFree pins the lock contract: Track,
// ClaimSave, snapshots and the save bookkeeping run concurrently without any
// external lock and stay race-free (run with -race).
func TestConcurrent_TrackClaimSnapshot_RaceFree(t *testing.T) {
	var s Store
	s.Track("sess-0")
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.Track(fmt.Sprintf("sess-%d", i))
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.ResetSaveThrottle()
			data, gen, due, err := s.ClaimSave(time.Now(), interval)
			if err != nil {
				t.Errorf("ClaimSave: %v", err)
				return
			}
			if due {
				var ids []string
				if err := json.Unmarshal(data, &ids); err != nil || !slices.IsSorted(ids) {
					t.Errorf("claimed bytes invalid: %v %q", err, data)
					return
				}
				s.MarkSavedIfUnchanged(gen)
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if got := s.SortedSnapshot(); !slices.IsSorted(got) {
				t.Errorf("unsorted snapshot %v", got)
				return
			}
			if _, err := s.MarshalSnapshot(); err != nil {
				t.Errorf("MarshalSnapshot: %v", err)
				return
			}
			_ = s.Has("sess-0")
			_ = s.Len()
			_ = s.Dirty()
			_ = s.Gen()
			_ = s.SavedAt()
		}
	}()

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	if got := len(s.order) - s.orderHead; got != s.Len() {
		t.Errorf("live window %d != Len %d after churn", got, s.Len())
	}
}
