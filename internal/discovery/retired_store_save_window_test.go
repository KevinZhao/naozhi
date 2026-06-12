package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestRetiredStore_SaveDoesNotSwallowConcurrentMark pins
// R20260610-085718-LB-6 (#2011): Save snapshots entries under the lock, then
// does the marshal + WriteFileAtomic (incl. fsync) UNLOCKED. A MarkRetired
// that lands in that window writes a NEW entry the in-flight write does not
// contain. Before the fix Save then cleared dirty=false unconditionally, so
// that entry was treated as persisted and the next ticker/shutdown Save was a
// no-op — losing it across a restart.
//
// The fix adds a mutation generation counter: Save only clears dirty when gen
// is unchanged across the unlocked write window.
//
// This is a concurrency stress regression. We hammer MarkRetired with unique
// sessionIDs while a flusher loop calls Save, then quiesce and Save once more.
// Every marked entry must survive on disk. With the bug, an entry that raced
// the write window vanishes (dirty wrongly cleared, final Save a no-op). Run
// under -race; many iterations reliably land inside the marshal+fsync window.
func TestRetiredStore_SaveDoesNotSwallowConcurrentMark(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "retired.json")
	rs, err := NewRetiredStore(path)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	const n = 2000
	base := time.UnixMilli(1700000000000)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Flusher: emulate the periodic ticker (server.go wiring) racing the
	// mutating lifecycle path.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = rs.Save()
			}
		}
	}()

	// Mutator: write n unique entries, then signal completion.
	mutDone := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			rs.MarkRetired(sid(i), base.Add(time.Duration(i)*time.Millisecond))
		}
		close(mutDone)
	}()
	<-mutDone

	// Stop the flusher and wait for it to exit before the final Save so the
	// final Save runs quiescent — the "flush the final retirement" shutdown
	// call the store godoc documents.
	close(stop)
	wg.Wait()

	// Final shutdown flush.
	if err := rs.Save(); err != nil {
		t.Fatalf("final Save: %v", err)
	}

	// Read the persisted file and confirm every in-memory entry is present.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted store: %v", err)
	}
	var file struct {
		Entries map[string]int64 `json:"entries"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	mem := rs.Snapshot()
	if len(file.Entries) != len(mem) {
		t.Fatalf("persisted entry count %d != in-memory %d; an entry that raced the Save write window was lost (dirty wrongly cleared)",
			len(file.Entries), len(mem))
	}
	for k, v := range mem {
		if got, ok := file.Entries[k]; !ok || got != v {
			t.Fatalf("entry %q missing/wrong on disk (disk=%d ok=%v, mem=%d); lost across the Save window",
				k, got, ok, v)
		}
	}
}

func sid(i int) string { return "sid-a-" + itoa(i) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
