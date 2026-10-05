package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/attachment"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/eventlog/ring"
	"github.com/naozhi/naozhi/internal/history/naozhilog"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// removeTeardownFixture is a router with the event log and attachment
// tracker on, a session at key whose Close blocks until released, and one
// persisted entry that references an attachment in ws.
type removeTeardownFixture struct {
	r        *Router
	dir      string
	ws       string
	key      string
	proc     *blockingCloseProc
	rel      string
	metaPath string
}

func newRemoveTeardownFixture(t *testing.T) *removeTeardownFixture {
	t.Helper()
	return newRemoveTeardownFixtureWith(t, func(p *blockingCloseProc) processIface { return p })
}

// newRemoveTeardownFixtureWith installs wrap(f.proc) as the session's process.
func newRemoveTeardownFixtureWith(t *testing.T, wrap func(*blockingCloseProc) processIface) *removeTeardownFixture {
	t.Helper()
	f := &removeTeardownFixture{ws: t.TempDir(), key: "dashboard:direct:alice:recreate"}
	f.r, f.dir = newEventLogRouter(t, false)
	now := time.Now().UTC()
	f.rel, f.metaPath = writeAttachmentPair(t, f.ws, now.Format("2006-01-02"), "img", now)
	f.proc = newBlockingCloseProc()
	installSession(t, f.r, f.key, wrap(f.proc)).setWorkspace(f.ws)
	f.write(t, "old")
	return f
}

// eventLogProc is a process with a real EventLog, so installPersistSink and
// the removal's sink detach reach it. A non-nil gate blocks EventLog until it
// closes, after closing reached.
type eventLogProc struct {
	*blockingCloseProc
	log     *ring.EventLog
	gate    chan struct{}
	reached chan struct{}
	once    sync.Once
}

func newEventLogProc(p *blockingCloseProc) *eventLogProc {
	return &eventLogProc{blockingCloseProc: p, log: ring.NewEventLog(16)}
}

func (p *eventLogProc) EventLog() *ring.EventLog {
	if p.gate != nil {
		p.once.Do(func() { close(p.reached) })
		<-p.gate
	}
	return p.log
}

// appendEntry appends uuid to log, referencing the fixture's attachment,
// and waits for whatever sink log has to reach disk and the tracker.
func (f *removeTeardownFixture) appendEntry(t *testing.T, log *ring.EventLog, uuid string) {
	t.Helper()
	log.Append(clievent.EventEntry{UUID: uuid, Type: "user", Summary: uuid, ImagePaths: []string{f.rel}})
	f.flush(t)
}

func (f *removeTeardownFixture) flush(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.r.hist.persister.Flush(ctx); err != nil {
		t.Fatalf("persister Flush: %v", err)
	}
	if err := f.r.hist.tracker.Flush(ctx); err != nil {
		t.Fatalf("tracker Flush: %v", err)
	}
}

// write persists one entry under key, as the session's installed sink would.
func (f *removeTeardownFixture) write(t *testing.T, uuid string) {
	t.Helper()
	sink := newEventLogSink(f.r.hist.persister.SinkFor(f.key), f.r.hist.tracker, persist.KeyHash(f.key))
	sink([]clievent.EventEntry{{UUID: uuid, Time: time.Now().UnixMilli(), Type: "user", Summary: uuid, ImagePaths: []string{f.rel}}}, false)
	f.flush(t)
}

// finishTeardown releases the blocked Close and waits for the teardown.
func (f *removeTeardownFixture) finishTeardown() {
	close(f.proc.release)
	<-f.proc.closeDone
	f.r.removes.Wait()
}

// warmStaleRing warms key's resident run-history ring with one run, then has
// a second store put another on disk behind it: a List that returns 1 run is
// served by that ring, 2 means the ring was freed and re-warmed from disk.
func (f *removeTeardownFixture) warmStaleRing(t *testing.T) {
	t.Helper()
	start := time.Now().Add(-time.Hour)
	run := func(id string, d int64) runhistory.SessionRun {
		return runhistory.SessionRun{RunID: id, SessionKey: f.key, StartedAt: start,
			EndedAt: start.Add(time.Duration(d) * time.Millisecond), DurationMS: d,
			Outcome: runhistory.OutcomeCompleted}
	}
	f.r.runs.runs.Append(run("00000000000000a1", 100))
	storePath := filepath.Join(filepath.Dir(f.dir), "sessions.json")
	other := runhistory.NewStore(datadir.ForStore(storePath).SessionRunsRoot(), 0, 0)
	other.Append(run("00000000000000a2", 200))
	other.Close()
	if got := f.r.Runs().List(f.key, 0, time.Time{}); len(got) != 1 {
		t.Fatalf("warmed ring lists %d runs, want 1", len(got))
	}
}

func (f *removeTeardownFixture) persistedUUIDs(t *testing.T) []string {
	t.Helper()
	got, err := naozhilog.New(f.dir, f.key).LoadLatest(context.Background(), 100)
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	var ids []string
	for _, e := range got {
		ids = append(ids, e.UUID)
	}
	return ids
}

func (f *removeTeardownFixture) attachmentReferenced(t *testing.T) bool {
	t.Helper()
	raw, err := os.ReadFile(f.metaPath)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	var m attachment.Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	return m.HasReference(persist.KeyHash(f.key))
}

// TestRemoveAsync_RecreatedDuringTeardown_LogHoldsOnlyNewSession: a session
// re-created on the key while the old one is still closing gets a log
// holding only its own entries, and its attachment refs survive the old
// teardown; the deleted conversation must not lead its history after restart.
func TestRemoveAsync_RecreatedDuringTeardown_LogHoldsOnlyNewSession(t *testing.T) {
	f := newRemoveTeardownFixture(t)
	if !f.r.RemoveAsync(f.key) {
		t.Fatal("RemoveAsync returned false")
	}
	select {
	case <-f.proc.closeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("teardown never reached proc.Close")
	}
	installSession(t, f.r, f.key, newIdleProc()).setWorkspace(f.ws)
	f.write(t, "new")

	f.finishTeardown()

	if ids := f.persistedUUIDs(t); !slices.Equal(ids, []string{"new"}) {
		t.Fatalf("persisted entries = %v, want [new]: the removed session's entries must not stay in the key's log", ids)
	}
	if !f.attachmentReferenced(t) {
		t.Fatal("the old teardown cleared the re-created session's attachment refs")
	}
}

// TestRemoveAsync_RecreateBeforeDrop_SinkWaitsForIt: a same-key session
// committed before the teardown reaches the drop binds its persist sink only
// once the drop is done, so the drop cannot take its entries and the old
// entries cannot lead them.
func TestRemoveAsync_RecreateBeforeDrop_SinkWaitsForIt(t *testing.T) {
	var old *eventLogProc
	f := newRemoveTeardownFixtureWith(t, func(p *blockingCloseProc) processIface {
		old = newEventLogProc(p)
		old.gate, old.reached = make(chan struct{}), make(chan struct{})
		return old
	})
	if !f.r.RemoveAsync(f.key) {
		t.Fatal("RemoveAsync returned false")
	}
	select {
	case <-old.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("teardown never reached the sink detach")
	}
	fresh := newEventLogProc(newBlockingCloseProc())
	close(fresh.release) // only the old process's Close blocks
	installSession(t, f.r, f.key, fresh).setWorkspace(f.ws)
	bound := make(chan struct{})
	go func() {
		defer close(bound)
		f.r.hist.installPersistSink(fresh, f.key)
	}()
	select {
	case <-bound:
		t.Fatal("installPersistSink returned while the old teardown had not dropped the log")
	case <-time.After(100 * time.Millisecond):
	}
	if fresh.log.SinkReady() {
		t.Fatal("re-created session's sink bound before the old teardown dropped the log")
	}

	close(old.gate)
	select {
	case <-bound:
	case <-time.After(5 * time.Second):
		t.Fatal("installPersistSink still waiting after the drop")
	}
	if !fresh.log.SinkReady() {
		t.Fatal("re-created session's sink not bound after the drop")
	}
	f.appendEntry(t, fresh.log, "new")
	f.finishTeardown()

	if ids := f.persistedUUIDs(t); !slices.Equal(ids, []string{"new"}) {
		t.Fatalf("persisted entries = %v, want [new]", ids)
	}
	if !f.attachmentReferenced(t) {
		t.Fatal("re-created session's attachment ref missing")
	}
}

// TestRemove_DetachesRemovedProcessSink: what the removed process emits while
// it closes is not written back under the dropped key.
func TestRemove_DetachesRemovedProcessSink(t *testing.T) {
	var old *eventLogProc
	f := newRemoveTeardownFixtureWith(t, func(p *blockingCloseProc) processIface {
		old = newEventLogProc(p)
		return old
	})
	f.r.hist.bindPersistSink(old.log, f.key)
	if !f.r.RemoveAsync(f.key) {
		t.Fatal("RemoveAsync returned false")
	}
	select {
	case <-f.proc.closeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("teardown never reached proc.Close")
	}
	f.appendEntry(t, old.log, "old-tail")
	f.finishTeardown()

	if _, err := os.Stat(persist.LogPath(f.dir, f.key)); !os.IsNotExist(err) {
		t.Fatalf("event log on disk after Remove (entries %v): err=%v", f.persistedUUIDs(t), err)
	}
	if f.attachmentReferenced(t) {
		t.Fatal("the removed process's tail re-referenced the attachment")
	}
}

// TestRemoveAsync_DropsLogWhenKeyStaysGone: with no same-key session in the
// table, the teardown still drops the event log and clears attachment refs.
func TestRemoveAsync_DropsLogWhenKeyStaysGone(t *testing.T) {
	f := newRemoveTeardownFixture(t)
	if !f.r.RemoveAsync(f.key) {
		t.Fatal("RemoveAsync returned false")
	}
	f.finishTeardown()

	if _, err := os.Stat(persist.LogPath(f.dir, f.key)); !os.IsNotExist(err) {
		t.Fatalf("event log still on disk after Remove: err=%v", err)
	}
	if f.attachmentReferenced(t) {
		t.Fatal("attachment refs not cleared after Remove")
	}
}

// TestRemoveAsync_KeepsRunHistoryRingOfSameKeySessionRecreatedDuringTeardown:
// the run-history ring and its per-owner lock belong to the re-created
// session, so the old teardown must not free them under it.
func TestRemoveAsync_KeepsRunHistoryRingOfSameKeySessionRecreatedDuringTeardown(t *testing.T) {
	f := newRemoveTeardownFixture(t)
	if !f.r.RemoveAsync(f.key) {
		t.Fatal("RemoveAsync returned false")
	}
	select {
	case <-f.proc.closeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("teardown never reached proc.Close")
	}
	installSession(t, f.r, f.key, newIdleProc()).setWorkspace(f.ws)
	f.warmStaleRing(t)

	f.finishTeardown()

	if got := f.r.Runs().List(f.key, 0, time.Time{}); len(got) != 1 {
		t.Fatalf("after teardown: %d runs, want the 1 in the re-created session's ring (ring freed?)", len(got))
	}
}

// TestRemoveAsync_FreesRunHistoryRingWhenKeyStaysGone: with no same-key
// session in the table, the teardown frees the key's resident ring.
func TestRemoveAsync_FreesRunHistoryRingWhenKeyStaysGone(t *testing.T) {
	f := newRemoveTeardownFixture(t)
	f.warmStaleRing(t)
	if !f.r.RemoveAsync(f.key) {
		t.Fatal("RemoveAsync returned false")
	}
	f.finishTeardown()

	if got := f.r.Runs().List(f.key, 0, time.Time{}); len(got) != 2 {
		t.Fatalf("after teardown: %d runs, want 2 re-warmed from disk (ring not freed?)", len(got))
	}
}
