package dispatch

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// fakeSessionRouter is a minimal SessionRouter implementation for
// Dispatcher tests. Methods marked "not configured" panic so a test
// that accidentally exercises an unexpected code path surfaces
// immediately rather than silently returning zero values.
//
// Usage: construct with the specific method closures your test needs;
// leave the rest at their default-panic behavior.
type fakeSessionRouter struct {
	getOrCreateCalls atomic.Int64

	getOrCreate func(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
	getSession  func(key string) *session.ManagedSession
	notifyIdle  func()
}

func (f *fakeSessionRouter) GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error) {
	f.getOrCreateCalls.Add(1)
	if f.getOrCreate == nil {
		panic("fakeSessionRouter.GetOrCreate not configured")
	}
	return f.getOrCreate(ctx, key, opts)
}

func (f *fakeSessionRouter) GetSession(key string) *session.ManagedSession {
	if f.getSession == nil {
		return nil
	}
	return f.getSession(key)
}

func (f *fakeSessionRouter) Reset(string)     { panic("fakeSessionRouter.Reset not configured") }
func (f *fakeSessionRouter) ResetChat(string) { panic("fakeSessionRouter.ResetChat not configured") }

func (f *fakeSessionRouter) GetWorkspace(string) string {
	panic("fakeSessionRouter.GetWorkspace not configured")
}

func (f *fakeSessionRouter) SetWorkspace(string, string) {
	panic("fakeSessionRouter.SetWorkspace not configured")
}

func (f *fakeSessionRouter) InterruptSessionViaControl(string) session.InterruptOutcome {
	panic("fakeSessionRouter.InterruptSessionViaControl not configured")
}

func (f *fakeSessionRouter) NotifyIdle() {
	if f.notifyIdle != nil {
		f.notifyIdle()
	}
}

// TestDispatcher_AcceptsFakeSessionRouter is the smoke test that
// proves the consumer-interfaces refactor actually lets tests swap in
// a fake router. Without it, this file would compile but nothing would
// demonstrate end-to-end injectability.
//
// Scope: only constructs a Dispatcher with a fakeSessionRouter and
// verifies router field assignment + structural typing holds. The
// handler-level IM flow (dispatch.BuildHandler → sendAndReply →
// router.GetOrCreate) is covered by existing dispatch_test.go via
// real Router; repeating it with a fake would duplicate coverage
// without adding signal. Future tests exercising narrow paths (e.g.
// an ErrMaxProcs user-message assertion) go in this file.
func TestDispatcher_AcceptsFakeSessionRouter(t *testing.T) {
	t.Parallel()

	fake := &fakeSessionRouter{
		getSession: func(string) *session.ManagedSession { return nil },
	}
	// Compile-time: fake satisfies SessionRouter.
	var _ SessionRouter = fake

	d := &Dispatcher{router: fake}

	// Runtime: routing calls reach the fake.
	if got := d.router.GetSession("any:key:foo:general"); got != nil {
		t.Errorf("expected nil session from fake, got %v", got)
	}
}

// TestFakeSessionRouter_UnconfiguredPanics locks the design choice
// documented above: fakes panic on unconfigured methods so tests can't
// accidentally pass by exercising paths that weren't asserted. If a
// future PR flips the panics to zero-value returns, this test goes
// red.
func TestFakeSessionRouter_UnconfiguredPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from unconfigured fake method, got nil")
		}
	}()
	fake := &fakeSessionRouter{}
	fake.Reset("any")
}

// TestNewDispatcher_NilRouterStaysUntypedNil pins the typed-nil fix:
// when DispatcherConfig.Router is nil, the Dispatcher.router
// interface field must hold untyped nil so subsequent
// `if d.router != nil` guards behave correctly (e.g.
// discardQueue at dispatch.go ~404). A naive assignment
// `d.router = cfg.Router` would store a typed-nil (*session.Router
// value nil wrapped in interface), making != nil return true and
// panicking on the next method call.
func TestNewDispatcher_NilRouterStaysUntypedNil(t *testing.T) {
	t.Parallel()
	d := NewDispatcher(DispatcherConfig{Router: nil})
	if d.router != nil {
		t.Fatal("Dispatcher.router should be untyped nil when cfg.Router is nil; typed-nil trap reintroduced")
	}
	// discardQueue with a nil router must be a no-op, not a panic.
	d.discardQueue("irrelevant:key:0:general")
}
