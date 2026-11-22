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
		// A relay is the user's words the gateway passed on, not their
		// message in the session: it does not lift a halt (§15.15 ②).
		{"a relay after it", []clievent.EventEntry{user(1000, ""), user(2000, clievent.SourceRelay)}, 1000},
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

// A turn the CLI ended as failed without the "API Error" prefix (its own
// retries ran out: "Request timed out") is a failure health marks and
// custody retries, not an idle session; the next turn's answer clears it.
func TestFailedTurnWithoutEnvelope(t *testing.T) {
	r := newDigestRouter(t)
	const key = "dashboard:direct:timeout:general"
	p := session.NewTestProcess()
	p.EventLog.Append(clievent.EventEntry{Time: time.Now().UnixMilli(), Type: "text", Summary: "Request timed out"})
	s := r.InjectSession(key, p)
	r.EndTurnForTest(s, clievent.SendResult{Text: "Request timed out", IsError: true, SubType: "success"})
	src := CustodySources{Router: r}
	if got := src.FailText(key); got != "Request timed out" {
		t.Errorf("FailText = %q, want the CLI's error", got)
	}
	f := sessionFact(s.Snapshot(), s, "")
	if f.Failed == "" || f.ErrText != "Request timed out" {
		t.Errorf("fact Failed = %q, ErrText = %q; want a failure", f.Failed, f.ErrText)
	}
	// A failed turn that wrote no assistant text (a kiro rejection, a
	// max-turns stop) is classified by its own error, not by the older
	// reply still in LastResponse.
	p.EventLog.Append(clievent.EventEntry{Time: time.Now().UnixMilli(), Type: "text", Summary: "上次的 403 forbidden 是 connection refused 引起的"})
	r.EndTurnForTest(s, clievent.SendResult{Text: "[kiro] session/prompt rejected", IsError: true})
	if got := src.FailText(key); got != "[kiro] session/prompt rejected" {
		t.Errorf("FailText = %q, want the failed turn's own error", got)
	}
	if f := sessionFact(s.Snapshot(), s, ""); f.Failed == "" || f.ErrText != "[kiro] session/prompt rejected" {
		t.Errorf("fact Failed = %q, ErrText = %q; want the failed turn's own error", f.Failed, f.ErrText)
	}
	r.EndTurnForTest(s, clievent.SendResult{Text: "好了"})
	if f := sessionFact(s.Snapshot(), s, ""); f.Failed != "" {
		t.Errorf("fact after an answer Failed = %q", f.Failed)
	}
}

// Dir is a session's working directory, the workspace a watched session's
// line to the Planner names; none for a key with no session here.
func TestCustodySourcesDir(t *testing.T) {
	r := newDigestRouter(t)
	src := CustodySources{Router: r}
	const key = "dashboard:direct:a:general"
	r.InjectSession(key, session.NewTestProcess()).SetWorkspaceForTest("/work/naozhi")
	if got := src.Dir(key); got != "/work/naozhi" {
		t.Errorf("Dir = %q, want the session's workspace", got)
	}
	if got := src.Dir("dashboard:direct:gone:general"); got != "" {
		t.Errorf("Dir of no session = %q", got)
	}
}
