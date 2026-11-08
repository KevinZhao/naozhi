package wireup

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/ctxutil"
	"github.com/naozhi/naozhi/internal/session"
)

// The user's last message is the latest "user" entry custody or the
// Planner did not send; a cross-session message is not one.
func TestLastUserAt(t *testing.T) {
	user := func(ms int64, src string) clievent.EventEntry {
		return clievent.EventEntry{Time: ms, Type: clievent.KindUser, Source: src}
	}
	for _, c := range []struct {
		name    string
		entries []clievent.EventEntry
		want    int64
	}{
		{"none", nil, 0},
		{"the user's", []clievent.EventEntry{user(1000, ""), {Time: 2000, Type: "text"}}, 1000},
		{"custody's after it", []clievent.EventEntry{user(1000, ""), user(2000, clievent.SourceCustody)}, 1000},
		{"the dispatch only", []clievent.EventEntry{user(1000, clievent.SourcePlanner), {Time: 2000, Type: clievent.KindPeerMessage}}, 0},
		{"the latest", []clievent.EventEntry{user(1000, ""), user(3000, "")}, 3000},
	} {
		got := lastUserAt(c.entries)
		if want := unixMilli(c.want); !got.Equal(want) {
			t.Errorf("%s: lastUserAt = %v, want %v", c.name, got, want)
		}
	}
}

// UserSince sees the user's message after a mark however long the turn it
// started ran: past the event log's tail, and past the ring itself. A
// message the gateway sent is not the user's.
func TestUserSince(t *testing.T) {
	r := newDigestRouter(t)
	src := CustodySources{Router: r}
	mark := time.Now().Add(-time.Minute)
	tools := func(p *session.TestProcess, n int) {
		for range n {
			p.EventLog.Append(clievent.EventEntry{Time: time.Now().UnixMilli(), Type: "tool_use"})
		}
	}

	// Sent through the session, then a turn longer than the ring.
	const sent = "dashboard:direct:a:general"
	p := session.NewTestProcess()
	s := r.InjectSession(sent, p)
	if _, err := s.Send(context.Background(), "再看一下", nil, nil); err != nil {
		t.Fatal(err)
	}
	tools(p, 1200)
	if !src.UserSince(sent, mark) {
		t.Error("sent then a long turn: UserSince = false")
	}
	if src.UserSince(sent, time.Now().Add(time.Second)) {
		t.Error("UserSince a time after the message = true")
	}

	// From the event log only (a restart): past the tail LastUser reads.
	const logged = "dashboard:direct:b:general"
	p = session.NewTestProcess()
	r.InjectSession(logged, p)
	p.EventLog.Append(clievent.EventEntry{Time: time.Now().UnixMilli(), Type: clievent.KindUser, Summary: "再看一下"})
	tools(p, userScan+44)
	if !src.LastUser(logged).IsZero() {
		t.Fatal("the fixture's message is still in LastUser's tail")
	}
	if !src.UserSince(logged, mark) {
		t.Error("logged then a long turn: UserSince = false")
	}

	// The gateway's message is not the user's.
	const relayed = "dashboard:direct:c:general"
	p = session.NewTestProcess()
	s = r.InjectSession(relayed, p)
	if _, err := s.Send(ctxutil.WithSource(context.Background(), clievent.SourceCustody), "继续", nil, nil); err != nil {
		t.Fatal(err)
	}
	p.EventLog.Append(clievent.EventEntry{Time: time.Now().UnixMilli(), Type: clievent.KindUser, Source: clievent.SourceCustody})
	if src.UserSince(relayed, mark) {
		t.Error("custody's message: UserSince = true")
	}
}

// UserSent is the user's word reaching the session after a time, typed or
// relayed by the gateway (their decision, their answer to a question):
// custody's message, the Planner's dispatch and a message before the time
// are not.
func TestUserSent(t *testing.T) {
	r := newDigestRouter(t)
	src := CustodySources{Router: r}
	since := time.Now().Add(-time.Minute)
	for _, c := range []struct {
		name   string
		source string
		at     time.Time
		want   bool
	}{
		{"typed", "", time.Now(), true},
		{"relayed", clievent.SourceRelay, time.Now(), true},
		{"custody's", clievent.SourceCustody, time.Now(), false},
		{"the dispatch", clievent.SourcePlanner, time.Now(), false},
		{"relayed before", clievent.SourceRelay, since.Add(-time.Minute), false},
	} {
		key := "task:" + strings.ReplaceAll(c.name, " ", "-")
		p := session.NewTestProcess()
		r.InjectSession(key, p)
		p.EventLog.Append(clievent.EventEntry{Time: c.at.UnixMilli(), Type: clievent.KindUser, Source: c.source, Summary: "选 A"})
		p.EventLog.Append(clievent.EventEntry{Time: time.Now().UnixMilli(), Type: "text"})
		if got := src.UserSent(key, since); got != c.want {
			t.Errorf("%s: UserSent = %v, want %v", c.name, got, c.want)
		}
	}
	if src.UserSent("task:none", since) {
		t.Error("no session: UserSent = true")
	}
}
