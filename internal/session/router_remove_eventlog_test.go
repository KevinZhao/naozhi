package session

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/attachment"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/history/naozhilog"
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
	f := &removeTeardownFixture{ws: t.TempDir(), key: "dashboard:direct:alice:recreate"}
	f.r, f.dir = newEventLogRouter(t, false)
	now := time.Now().UTC()
	f.rel, f.metaPath = writeAttachmentPair(t, f.ws, now.Format("2006-01-02"), "img", now)
	f.proc = newBlockingCloseProc()
	installSession(t, f.r, f.key, f.proc).setWorkspace(f.ws)
	f.write(t, "old")
	return f
}

// write persists one entry under key, as the session's installed sink would.
func (f *removeTeardownFixture) write(t *testing.T, uuid string) {
	t.Helper()
	sink := newEventLogSink(f.r.hist.persister.SinkFor(f.key), f.r.hist.tracker, persist.KeyHash(f.key))
	sink([]clievent.EventEntry{{UUID: uuid, Time: time.Now().UnixMilli(), Type: "user", Summary: uuid, ImagePaths: []string{f.rel}}}, false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.r.hist.persister.Flush(ctx); err != nil {
		t.Fatalf("persister Flush: %v", err)
	}
	if err := f.r.hist.tracker.Flush(ctx); err != nil {
		t.Fatalf("tracker Flush: %v", err)
	}
}

// finishTeardown releases the blocked Close and waits for the teardown.
func (f *removeTeardownFixture) finishTeardown() {
	close(f.proc.release)
	<-f.proc.closeDone
	f.r.removes.Wait()
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

// TestRemoveAsync_KeepsLogOfSameKeySessionRecreatedDuringTeardown: a session
// re-created on the key while the old one is still closing owns the key's
// event log and attachment refs, so the old teardown leaves both in place.
func TestRemoveAsync_KeepsLogOfSameKeySessionRecreatedDuringTeardown(t *testing.T) {
	f := newRemoveTeardownFixture(t)
	if !f.r.RemoveAsync(f.key) {
		t.Fatal("RemoveAsync returned false")
	}
	installSession(t, f.r, f.key, newIdleProc()).setWorkspace(f.ws)
	f.write(t, "new")

	f.finishTeardown()

	ids := f.persistedUUIDs(t)
	if !slices.Contains(ids, "new") {
		t.Fatalf("persisted entries = %v; the old teardown dropped the re-created session's log", ids)
	}
	if !f.attachmentReferenced(t) {
		t.Fatal("the old teardown cleared the re-created session's attachment refs")
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
