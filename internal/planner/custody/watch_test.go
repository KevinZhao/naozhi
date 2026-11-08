package custody

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// Observing is on by default for this node's sessions the user opened (a
// dashboard chat, an IM chat, a project planner chat, a taken-over CLI) and
// never for the rest; the switch is its own, beside custody's, written to
// the sidecar only once set, the Planner's noted and the user's not.
func TestWatchDefaultAndSet(t *testing.T) {
	r := newRig(t)
	r.dispatch("task:a")
	const im = "feishu:group:oc_1#t9:general"
	for _, tc := range []struct {
		key  string
		want bool
	}{
		{dash, true}, {im, true}, {"project:naozhi:planner", true}, {"local:takeover:/w:general", true},
		{"task:a", false}, {"cron:nightly", false}, {"scratch:x", false}, {sessionkey.ScoutKeyPrefix + "r", false},
		{"sys:attachment-gc", false}, {sessionkey.PlannerMainKey, false}, {"remote/" + dash, false},
	} {
		if got := r.e.Watched(tc.key); got != tc.want {
			t.Errorf("Watched(%s) = %v, want %v", tc.key, got, tc.want)
		}
	}
	// The default writes nothing.
	if _, ok := r.sessions.Get(dash); ok {
		t.Error("the default gave the session a sidecar")
	}
	if st := r.e.Status(dash); !st.Watch || st.WatchBy != "" {
		t.Errorf("Status(dash) watch = %v by %q, want the default on", st.Watch, st.WatchBy)
	}
	if got := chats(r.e.List()); len(got) != 0 {
		t.Errorf("List = %+v, want no chat: observing on is the default", got)
	}

	for _, tc := range []struct {
		key  string
		want error
	}{
		{"task:a", ErrRefused}, {"cron:nightly", ErrRefused}, {"scratch:x", ErrRefused},
		{sessionkey.PlannerMainKey, ErrRefused}, {"remote/" + dash, ErrNoSession},
	} {
		if err := r.e.SetWatch(tc.key, false, ByUser); !errors.Is(err, tc.want) {
			t.Errorf("SetWatch(%s) = %v, want %v", tc.key, err, tc.want)
		}
	}

	if err := r.e.SetWatch(dash, false, ByUser); err != nil {
		t.Fatal(err)
	}
	if r.e.Watched(dash) {
		t.Error("Watched after the user turned it off")
	}
	m, _ := r.sessions.Get(dash)
	if m.Watch == nil || m.Watch.On || m.Watch.By != ByUser || !m.Watch.At.Equal(r.now) || m.Custody != nil {
		t.Errorf("sidecar watch %+v custody %+v, want off by the user, custody untouched", m.Watch, m.Custody)
	}
	if got := r.custodyEvents(dash); len(got) != 0 {
		t.Errorf("journal = %v, want none for the user's switch", got)
	}
	// Turned off, the session is listed so the page reads it off.
	if got := chats(r.e.List()); len(got) != 1 || got[0].Key != dash || got[0].Watch || got[0].WatchBy != ByUser {
		t.Errorf("List = %+v, want dash with observing off", got)
	}
	// Custody is a separate switch.
	if err := r.e.Set(dash, true, ByUser); err != nil {
		t.Fatal(err)
	}
	if !r.e.On(dash) || r.e.Watched(dash) {
		t.Errorf("custody on: On %v Watched %v, want on and still not observed", r.e.On(dash), r.e.Watched(dash))
	}

	// The Planner's switch is noted; one that changes nothing is not.
	if err := r.e.SetWatch(im, false, ByPlanner); err != nil {
		t.Fatal(err)
	}
	if err := r.e.SetWatch(im, false, ByPlanner); err != nil {
		t.Fatal(err)
	}
	if got := r.custodyEvents(im); len(got) != 1 || !strings.Contains(got[0], `"event":"watch_off"`) || !strings.Contains(got[0], `"by":"planner"`) {
		t.Errorf("journal = %v, want one watch_off by the Planner", got)
	}
	if err := r.e.SetWatch(im, true, ByPlanner); err != nil {
		t.Fatal(err)
	}
	if got := r.custodyEvents(im); len(got) != 2 || !strings.Contains(got[1], `"event":"watch_on"`) {
		t.Errorf("journal = %v, want watch_on after it", got)
	}

	r.open()
	if r.e.Watched(dash) || !r.e.Watched(im) {
		t.Error("the switch did not survive a restart")
	}

	// With the Planner off nothing is observed.
	r.e.cfg.Disabled = true
	if r.e.Watched(im) || r.e.SetWatch(im, false, ByUser) == nil {
		t.Error("observing with the Planner off")
	}
}

// A watched session's turn that ends with the user away is a result
// (ResultAt); one that ends with them at it marks a pending result seen;
// one that ends asking them is 等你, no result; an unwatched one, a task:
// session and the Planner get nothing.
func TestEndedMarksResult(t *testing.T) {
	r := newRig(t)
	r.dispatch("task:a")

	// The user is away: a result, and a sidecar to hold it.
	r.e.ended(dash, r.now)
	m, ok := r.sessions.Get(dash)
	if !ok || !m.ResultAt.Equal(r.now) || m.Purpose != "" || m.Custody != nil {
		t.Fatalf("sidecar %+v (%v), want ResultAt now, nothing else set", m, ok)
	}

	// Sent it a message a minute ago: at it, the result is seen.
	r.now = r.now.Add(10 * time.Minute)
	r.lastUser[dash] = r.now.Add(-time.Minute)
	r.e.ended(dash, r.now)
	m, _ = r.sessions.Get(dash)
	if !m.SeenAt.Equal(r.now) || m.ResultAt.After(m.SeenAt) {
		t.Errorf("at it: ResultAt %v SeenAt %v, want the result seen now", m.ResultAt, m.SeenAt)
	}
	seen := m.SeenAt

	// Viewing it, nothing pending: nothing written.
	r.now = r.now.Add(10 * time.Minute)
	r.e.SetViewing(dash, "tab1", true)
	r.e.ended(dash, r.now)
	if m, _ = r.sessions.Get(dash); !m.SeenAt.Equal(seen) {
		t.Errorf("viewing with nothing new: SeenAt %v, want %v kept", m.SeenAt, seen)
	}
	r.e.SetViewing(dash, "tab1", false)

	// Away, with a question left for the user: 等你, not a result.
	r.now = r.now.Add(10 * time.Minute)
	r.asking[dash] = true
	r.e.ended(dash, r.now)
	if m, _ = r.sessions.Get(dash); m.ResultAt.After(seen) {
		t.Errorf("asking: ResultAt %v, want no result", m.ResultAt)
	}
	r.asking[dash] = false

	// Not observed: nothing.
	const im = "slack:direct:u1:general"
	if err := r.e.SetWatch(im, false, ByUser); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{im, "task:a", sessionkey.PlannerMainKey, "cron:nightly", "remote/" + dash} {
		before, _ := r.sessions.Get(key)
		r.e.ended(key, r.now)
		if after, _ := r.sessions.Get(key); !after.ResultAt.Equal(before.ResultAt) {
			t.Errorf("Ended(%s) wrote ResultAt %v", key, after.ResultAt)
		}
	}
	if _, ok := r.sessions.Get(sessionkey.PlannerMainKey); ok {
		t.Error("Ended gave the Planner a sidecar")
	}

	// Away again: a new result.
	r.e.ended(dash, r.now)
	if m, _ = r.sessions.Get(dash); !m.ResultAt.Equal(r.now) || !m.ResultAt.After(m.SeenAt) {
		t.Errorf("away again: ResultAt %v SeenAt %v, want a new result now", m.ResultAt, m.SeenAt)
	}
}

// chats are the statuses of sessions not dispatched.
func chats(l []Status) []Status {
	var out []Status
	for _, st := range l {
		if !sessionkey.IsTaskKey(st.Key) {
			out = append(out, st)
		}
	}
	return out
}

// Ended runs on a CLI's read loop: it queues the turn end and returns while
// a delivery holds the engine's lock, and Run's worker marks the result
// with the time the turn ended. With the Planner off nothing is queued.
func TestEndedQueues(t *testing.T) {
	r := newRig(t)
	ended := r.now
	r.e.mu.Lock()
	done := make(chan struct{})
	go func() { r.e.Ended(dash); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		r.e.mu.Unlock()
		t.Fatal("Ended waited on the engine's lock")
	}
	r.e.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.e.runEnds(ctx)
	testhelper.Eventually(t, func() bool {
		m, ok := r.sessions.Get(dash)
		return ok && m.ResultAt.Equal(ended)
	}, 5*time.Second, "the queued turn end never marked a result")

	r.e.cfg.Disabled = true
	r.e.Ended(dash)
	if n := len(r.e.ends); n != 0 {
		t.Errorf("Planner off: %d turn ends queued", n)
	}
}

// A watched session whose sidecar a newer naozhi wrote is left alone, and
// quietly: its every turn end would warn otherwise.
func TestEndedNewerSidecar(t *testing.T) {
	r := newRig(t)
	if err := os.MkdirAll(filepath.Join(r.dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	newer := filepath.Join(r.dir, "sessions", sidecarName(dash))
	if err := os.WriteFile(newer, []byte(`{"v":99,"key":"`+dash+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r.open()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)
	r.e.ended(dash, r.now)
	if logs.Len() != 0 {
		t.Errorf("warned: %s", logs.String())
	}
	if data, _ := os.ReadFile(newer); !strings.Contains(string(data), `"v":99`) || strings.Contains(string(data), "result_at") {
		t.Errorf("newer sidecar rewritten: %s", data)
	}
}

// sidecarName is the sidecar file of key (planner/sidecar's naming).
func sidecarName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]) + ".json"
}
