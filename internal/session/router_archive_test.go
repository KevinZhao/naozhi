package session

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
)

// An archived key leaves the table and its process closes, but its event log
// stays for the resume under the same key; Remove drops it.
func TestArchiveKeepsEventLog(t *testing.T) {
	r, dir := newEventLogRouter(t)
	const archived, removed = "task:aaaaaaaaaaaa", "task:bbbbbbbbbbbb"
	procs := map[string]*TestProcess{}
	for _, key := range []string{archived, removed} {
		p := NewTestProcess()
		procs[key] = p
		s := r.InjectSession(key, p)
		s.setSessionID("sid-" + key)
		s.SetWorkspaceForTest("/w")
		storeAtomicString(&s.lastPrompt, "do it")
		sink := newEventLogSink(r.hist.persister.SinkFor(key), nil, "")
		sink([]clievent.EventEntry{{UUID: "u-" + key, Time: 1, Type: "user"}}, false)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = r.hist.persister.Flush(ctx)

	got, ok := r.Archive(archived)
	if !ok || got.SessionID != "sid-"+archived || got.Workspace != "/w" || got.LastPrompt != "do it" {
		t.Fatalf("Archive = %+v, %v", got, ok)
	}
	if r.SessionFor(archived) != nil {
		t.Fatal("archived session still in the table")
	}
	if procs[archived].Alive() {
		t.Error("archived session's process not closed")
	}
	if _, err := os.Stat(persist.LogPath(dir, archived)); err != nil {
		t.Errorf("archived session's event log gone: %v", err)
	}
	if _, ok := r.Archive(archived); ok {
		t.Error("second Archive of the same key reported true")
	}

	r.Remove(removed)
	if _, err := os.Stat(persist.LogPath(dir, removed)); !os.IsNotExist(err) {
		t.Errorf("removed session's event log kept: %v", err)
	}

	if k := r.RegisterForResume(archived, got.SessionID, got.Workspace, got.LastPrompt); k != archived {
		t.Fatalf("RegisterForResume = %q", k)
	}
	if s := r.SessionFor(archived); s == nil || s.SessionID() != "sid-"+archived {
		t.Fatal("archived session not back under its key with its session ID")
	}
}
