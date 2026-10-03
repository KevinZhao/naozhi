package server

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
	"github.com/naozhi/naozhi/internal/turn"
)

// recordingNotifier records every sendNotifier call in order.
type recordingNotifier struct {
	mu    sync.Mutex
	calls []string
}

func (n *recordingNotifier) add(s string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, s)
}

func (n *recordingNotifier) BroadcastSessionReady(key string) { n.add("ready " + key) }
func (n *recordingNotifier) BroadcastSessionsUpdate()         { n.add("sessions") }
func (n *recordingNotifier) broadcastState(key, state, _ string) {
	n.add("state " + key + " " + state)
}
func (n *recordingNotifier) broadcastSendError(key, msg string) { n.add("error " + key + " " + msg) }

func (n *recordingNotifier) snapshot() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return strings.Join(n.calls, "|")
}

// TestTurnSender_FailedCreateIsNilInterface: when the router cannot produce
// a session the orchestrator gets a nil interface and the router's error, not
// a typed nil it would hand on to Send.
func TestTurnSender_FailedCreateIsNilInterface(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	sess, _, err := (turnSender{router: r}).GetOrCreate(context.Background(), "feishu:direct:absent:general", sessionview.AgentOpts{})
	if err == nil {
		t.Fatal("GetOrCreate on a router with no CLI wrapper = nil error, want the router's refusal")
	}
	if sess != nil {
		t.Errorf("GetOrCreate failure returned %#v, want a nil interface", sess)
	}
}

// TestTurnSender_ALiveSessionComesThrough: an existing session is the one
// the orchestrator gets, and so the one Send later receives.
func TestTurnSender_ALiveSessionComesThrough(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	const key = "feishu:direct:present:general"
	want := r.InjectSession(key, session.NewTestProcess())
	got, status, err := (turnSender{router: r}).GetOrCreate(context.Background(), key, sessionview.AgentOpts{})
	if err != nil {
		t.Fatalf("GetOrCreate(present): %v", err)
	}
	if ms, ok := got.(*session.ManagedSession); !ok || ms != want {
		t.Errorf("GetOrCreate(present) = %#v, want the router's session", got)
	}
	if status != sessionview.SessionExisting {
		t.Errorf("status = %v, want SessionExisting", status)
	}
}

type foreignSession struct{}

func (foreignSession) Backend() string { return "claude" }

// TestTurnSender_SendRefusesAForeignSession: Send only accepts the session
// GetOrCreate produced; anything else is reported, not dereferenced.
func TestTurnSender_SendRefusesAForeignSession(t *testing.T) {
	s := turnSender{notify: &recordingNotifier{}}
	for _, sess := range []turn.Session{foreignSession{}, nil, (*session.ManagedSession)(nil)} {
		_, err := s.Send(context.Background(), "k", sess, "hi", nil, turn.SendSpec{}, nil)
		if err == nil || !strings.Contains(err.Error(), "did not produce") {
			t.Errorf("Send(%T) err = %v, want the wiring-fault error", sess, err)
		}
	}
}

// TestTurnSender_SendHonoursSpec: SendSpec, not the ctx, picks the path —
// passthrough only when the session supports it, and PriorityNow without
// passthrough interrupts the in-flight turn before sending. Every send is
// preceded by the running broadcast and AfterTurn follows with the state and
// sessions snapshot.
func TestTurnSender_SendHonoursSpec(t *testing.T) {
	cases := []struct {
		name           string
		supports       bool
		spec           turn.SendSpec
		wantPath       string
		wantInterrupts int
	}{
		{"passthrough on a passthrough session", true, turn.SendSpec{Passthrough: true}, "passthrough ", 0},
		{"passthrough asked, session cannot", false, turn.SendSpec{Passthrough: true}, "send", 0},
		{"normal", true, turn.SendSpec{}, "send", 0},
		{"urgent passthrough", true, turn.SendSpec{Passthrough: true, Priority: turn.PriorityNow}, "passthrough now", 0},
		{"urgent without passthrough", false, turn.SendSpec{Passthrough: true, Priority: turn.PriorityNow}, "send", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
			t.Cleanup(r.Shutdown)
			const key = "feishu:direct:spec:general"
			var path string
			interrupts := 0
			proc := session.NewTestProcess()
			proc.PassthroughVal = tc.supports
			proc.SendFunc = func(context.Context, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error) {
				path = "send"
				return &clievent.SendResult{Text: "ok"}, nil
			}
			proc.SendPassthroughFunc = func(_ context.Context, _ string, _ []clievent.Attachment, _ clievent.EventCallback, priority string) (*clievent.SendResult, error) {
				path = "passthrough " + priority
				return &clievent.SendResult{Text: "ok"}, nil
			}
			proc.InterruptViaControlFunc = func() error { interrupts++; return nil }
			sess := r.InjectSession(key, proc)
			n := &recordingNotifier{}
			s := turnSender{router: r, notify: n}

			if _, err := s.Send(context.Background(), key, sess, "hi", nil, tc.spec, nil); err != nil {
				t.Fatal(err)
			}
			s.AfterTurn(key)
			if path != tc.wantPath || interrupts != tc.wantInterrupts {
				t.Errorf("path = %q, interrupts = %d; want %q, %d", path, interrupts, tc.wantPath, tc.wantInterrupts)
			}
			want := "ready " + key + "|state " + key + " " + sess.Snapshot().State + "|sessions"
			if got := n.snapshot(); got != want {
				t.Errorf("broadcasts = %q, want %q", got, want)
			}
		})
	}
}

// TestTurnSender_ResetKeepsOrDropsOverride: Reset(key, false) keeps the
// chat's workspace override (IM /new), Reset(key, true) drops it with the
// session in one step.
func TestTurnSender_ResetKeepsOrDropsOverride(t *testing.T) {
	for _, discard := range []bool{false, true} {
		r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
		t.Cleanup(r.Shutdown)
		const chatKey, key = "feishu:direct:reset", "feishu:direct:reset:general"
		dir := t.TempDir()
		r.SetWorkspace(chatKey, dir)
		r.InjectSession(key, session.NewTestProcess())

		turnSender{router: r}.Reset(key, discard)

		if r.SessionFor(key) != nil {
			t.Errorf("discardOverride=%v: session survived Reset", discard)
		}
		kept := r.Workspace(chatKey) == dir
		if kept == discard {
			t.Errorf("discardOverride=%v: workspace override kept=%v, want %v", discard, kept, !discard)
		}
	}
}
