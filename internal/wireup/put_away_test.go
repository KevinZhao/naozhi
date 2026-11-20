package wireup

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
)

// A dispatched session put away keeps its event log, and its record reads
// from it in pages, newest first; a key with no put away on its sidecar (a
// live task, any other session) is not read at all (D136).
func TestPutAwayRecord(t *testing.T) {
	const key, live = "task:aaaaaaaaaaaa", "task:bbbbbbbbbbbb"
	dir := t.TempDir()
	p, err := persist.NewPersister(persist.Options{Dir: dir, IdxStride: 2, FlushInterval: 10 * time.Millisecond, ChannelBuffer: 64, Generator: "put-away-test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{key, live} {
		sink := p.SinkFor(k)
		for i := 1; i <= 5; i++ {
			buf, _ := json.Marshal(clievent.EventEntry{UUID: fmt.Sprintf("%s-%d", k, i), Time: int64(i * 1000), Type: "text", Summary: fmt.Sprintf("第 %d 条", i)})
			sink([]persist.Entry{{JSON: buf, TimeMS: int64(i * 1000)}}, false)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	sc := sidecar.Open(t.TempDir(), nil)
	for _, k := range []string{key, live} {
		if err := sc.Put(sidecar.Meta{Key: k, Purpose: sidecar.PurposeTask, Title: k}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sc.Update(key, func(m *sidecar.Meta) bool { m.Reap = &sidecar.Reap{At: time.Now()}; return true }); err != nil {
		t.Fatal(err)
	}
	rec := PutAwayRecord{Sidecars: sc, EventsDir: dir}

	uuids := func(es []clievent.EventEntry) string {
		out := ""
		for _, e := range es {
			out += e.UUID[len(key)+1:]
		}
		return out
	}
	latest, more, ok := rec.Events(context.Background(), key, 0, 3)
	if !ok || !more || uuids(latest) != "345" {
		t.Fatalf("latest page: %q more %v ok %v", uuids(latest), more, ok)
	}
	older, more, ok := rec.Events(context.Background(), key, latest[0].Time, 3)
	if !ok || more || uuids(older) != "12" {
		t.Fatalf("older page: %q more %v ok %v", uuids(older), more, ok)
	}
	all, more, ok := rec.Events(context.Background(), key, 0, 50)
	if !ok || more || uuids(all) != "12345" {
		t.Fatalf("whole record: %q more %v ok %v", uuids(all), more, ok)
	}

	for _, k := range []string{live, "dashboard:direct:x:general", "task:nope"} {
		if es, _, ok := rec.Events(context.Background(), k, 0, 50); ok || len(es) != 0 {
			t.Errorf("%s is not put away, yet read: %d entries, ok %v", k, len(es), ok)
		}
	}
	if _, _, ok := (PutAwayRecord{}).Events(context.Background(), key, 0, 50); ok {
		t.Error("no store, no directory: read")
	}
}
