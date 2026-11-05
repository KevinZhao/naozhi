package note

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/journal"
)

// List derives a session's notes from the journal's health and notify
// entries, the dispatch and the reports, oldest first, within Window; a
// reopened journal (a restart) lists the same, and each journaled note is
// pushed as it is appended with the same ID.
func TestList(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "log")
	j, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	at := func(min int) time.Time { return now.Add(time.Duration(min) * time.Minute) }
	cfg := Config{
		Item: func(id string) (string, bool) {
			return "title " + id, id == "in_open" || id == "in_shut"
		},
		Dispatched: func(key string) time.Time {
			if key == "task:a" {
				return at(-90)
			}
			return time.Time{}
		},
		Reports: func(key string) []time.Time {
			if key == "task:a" {
				return []time.Time{at(-5), now.Add(-Window - time.Hour)}
			}
			return nil
		},
	}
	cfg.Journal = j
	n := New(cfg)
	var pushed []Note
	n.SetPush(func(nt Note) { pushed = append(pushed, nt) })

	appendE := func(min int, kind journal.Kind, unit, data string) {
		t.Helper()
		if err := j.Append(journal.Entry{TS: at(min), Kind: kind, Unit: unit, Data: json.RawMessage(data)}); err != nil {
			t.Fatal(err)
		}
	}
	appendE(-80, journal.KindHealth, "task:a", `{"from":"unknown","to":"active"}`)
	appendE(-70, journal.KindHealth, "task:a", `{"from":"active","to":"waiting","evidence":"unanswered AskUserQuestion"}`)
	appendE(-60, journal.KindNotify, "task:a", `{"event":"open","id":"in_shut","kind":"question"}`)
	appendE(-60, journal.KindNotify, "task:a", `{"event":"push","id":"in_shut"}`)
	appendE(-50, journal.KindHealth, "task:a", `{"from":"waiting","to":"idle"}`)
	appendE(-50, journal.KindNotify, "task:a", `{"event":"close","id":"in_shut","kind":"question","closed_by":"auto"}`)
	appendE(-40, journal.KindHealth, "task:a", `{"from":"idle","to":"failed","error_class":"rate_limit","evidence":"exit 1"}`)
	appendE(-40, journal.KindNotify, "task:a", `{"event":"open","id":"in_open","kind":"error"}`)
	appendE(-30, journal.KindHealth, "task:a", `{"from":"active","to":"idle"}`)
	appendE(-30, journal.KindHealth, "task:a", `{"from":"failed","to":"unknown"}`)
	appendE(-20, journal.KindHealth, "systemic:rate_limit", `{"systemic":true,"error_class":"rate_limit","units":["task:a","b"]}`)
	appendE(-20, journal.KindHealth, "b", `{"from":"active","to":"stalled"}`)
	appendE(-10, journal.KindActivity, "task:a", `{"type":"output"}`)

	type row struct {
		Kind               Kind
		From, To, ErrClass string
		Event, Item        string
		ItemKind, ClosedBy string
		Title              string
		Time               time.Time
	}
	rows := func(ns []Note) []row {
		out := make([]row, len(ns))
		for i, nt := range ns {
			out[i] = row{nt.Kind, nt.From, nt.To, nt.ErrClass, nt.Event, nt.Item, nt.ItemKind, nt.ClosedBy, nt.Title, nt.Time.UTC()}
		}
		return out
	}
	want := []row{
		{Kind: KindDispatch, Time: at(-90).UTC()},
		{Kind: KindHealth, From: "active", To: "waiting", Time: at(-70).UTC()},
		{Kind: KindInbox, Event: "open", Item: "in_shut", ItemKind: "question", Title: "title in_shut", Time: at(-60).UTC()},
		{Kind: KindHealth, From: "waiting", To: "idle", Time: at(-50).UTC()},
		{Kind: KindInbox, Event: "close", Item: "in_shut", ItemKind: "question", ClosedBy: "auto", Title: "title in_shut", Time: at(-50).UTC()},
		{Kind: KindHealth, From: "idle", To: "failed", ErrClass: "rate_limit", Time: at(-40).UTC()},
		{Kind: KindInbox, Event: "open", Item: "in_open", ItemKind: "error", Title: "title in_open", Time: at(-40).UTC()},
		{Kind: KindReport, Time: at(-5).UTC()},
	}
	got := n.List("task:a", now)
	if !slices.Equal(rows(got), want) {
		t.Fatalf("notes =\n%+v\nwant\n%+v", rows(got), want)
	}
	ids := map[string]bool{}
	for _, nt := range got {
		if nt.ID == "" || ids[nt.ID] || nt.Key != "task:a" {
			t.Errorf("note %+v: want a unique ID and key task:a", nt)
		}
		ids[nt.ID] = true
	}

	// Pushed: the journaled notes of every unit, with the IDs List gives.
	var pushedA []string
	for _, nt := range pushed {
		if nt.Key == "task:a" {
			pushedA = append(pushedA, nt.ID)
		}
	}
	var listedA []string
	for _, nt := range got {
		if nt.Kind == KindHealth || nt.Kind == KindInbox {
			listedA = append(listedA, nt.ID)
		}
	}
	if !slices.Equal(pushedA, listedA) || len(pushed) != len(pushedA)+1 {
		t.Errorf("pushed %+v, want task:a's %v and b's stall", pushed, listedA)
	}

	// A restart reads the same notes back.
	j.Close()
	j2, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Journal = j2
	if again := New(cfg).List("task:a", now); !slices.Equal(again, got) {
		t.Errorf("after reopen =\n%+v\nwant\n%+v", again, got)
	}
	if got := New(cfg).List("b", now.Add(Window)); len(got) != 0 {
		t.Errorf("notes past the window = %+v, want none", got)
	}
}
