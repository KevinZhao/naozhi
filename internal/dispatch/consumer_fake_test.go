package dispatch

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// Compile-time pin (ARCH-DISP-1, #457): the production *project.Manager
// must satisfy the ProjectStore consumer interface. If a method signature
// drifts on either side, this line fails to compile in the dispatch
// package's own test build, before any consumer wiring runs.
var _ ProjectStore = (*project.Manager)(nil)

// fakeProjectStore is a minimal ProjectStore for slash-command handler
// tests. Like fakeSessionRouter, unconfigured methods panic so an
// unexpected code path surfaces immediately.
type fakeProjectStore struct {
	get            func(name string) *project.Project
	all            func() []*project.Project
	projectForChat func(platform, chatType, chatID string) *project.Project
	bindChat       func(projectName, platform, chatType, chatID string) error
	unbindAllChat  func(platform, chatType, chatID string) error
}

func (f *fakeProjectStore) Get(name string) *project.Project {
	if f.get == nil {
		panic("fakeProjectStore.Get not configured")
	}
	return f.get(name)
}

func (f *fakeProjectStore) All() []*project.Project {
	if f.all == nil {
		panic("fakeProjectStore.All not configured")
	}
	return f.all()
}

func (f *fakeProjectStore) ProjectForChat(platform, chatType, chatID string) *project.Project {
	if f.projectForChat == nil {
		panic("fakeProjectStore.ProjectForChat not configured")
	}
	return f.projectForChat(platform, chatType, chatID)
}

func (f *fakeProjectStore) BindChat(projectName, platform, chatType, chatID string) error {
	if f.bindChat == nil {
		panic("fakeProjectStore.BindChat not configured")
	}
	return f.bindChat(projectName, platform, chatType, chatID)
}

func (f *fakeProjectStore) UnbindAllChat(platform, chatType, chatID string) error {
	if f.unbindAllChat == nil {
		panic("fakeProjectStore.UnbindAllChat not configured")
	}
	return f.unbindAllChat(platform, chatType, chatID)
}

// TestDispatcher_AcceptsFakeProjectStore proves the ProjectStore seam
// (ARCH-DISP-1, #457) lets slash-command tests inject a fake binding
// store without standing up a real project.Manager (projects.root dir +
// binding file). It exercises the read path (ProjectForChat) end-to-end
// through the interface field.
func TestDispatcher_AcceptsFakeProjectStore(t *testing.T) {
	t.Parallel()

	want := &project.Project{Name: "demo", Path: "/tmp/demo"}
	fake := &fakeProjectStore{
		projectForChat: func(_, _, _ string) *project.Project { return want },
	}
	var _ ProjectStore = fake

	d := &Dispatcher{projectMgr: fake}
	if got := d.projectMgr.ProjectForChat("im", "direct", "u1"); got != want {
		t.Errorf("expected injected project %v, got %v", want, got)
	}
}

// TestNewDispatcher_NilProjectMgrStaysUntypedNil pins the typed-nil fix
// for ProjectStore (ARCH-DISP-1, #457): cfg.ProjectMgr is a concrete
// *project.Manager, so a nil value boxed into the interface field would
// be != nil and defeat every `d.projectMgr == nil` gate. NewDispatcher
// must collapse it to untyped nil.
func TestNewDispatcher_NilProjectMgrStaysUntypedNil(t *testing.T) {
	t.Parallel()
	d, err := NewDispatcher(DispatcherConfig{ProjectMgr: nil, AllowMissingSender: true})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	if d.projectMgr != nil {
		t.Fatal("Dispatcher.projectMgr should be untyped nil when cfg.ProjectMgr is nil; typed-nil trap reintroduced")
	}
}

// fakeSessionRouter is a minimal SessionRouter implementation for
// Dispatcher tests. Methods marked "not configured" panic so a test
// that accidentally exercises an unexpected code path surfaces
// immediately rather than silently returning zero values.
//
// Usage: construct with the specific method closures your test needs;
// leave the rest at their default-panic behavior.
type fakeSessionRouter struct {
	getOrCreateCalls atomic.Int64

	getOrCreate               func(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
	notifyIdle                func()
	discardPassthroughPending func(key string, reason error)
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

func (f *fakeSessionRouter) DiscardPassthroughPending(key string, reason error) {
	if f.discardPassthroughPending != nil {
		f.discardPassthroughPending(key, reason)
	}
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

	var notified int
	fake := &fakeSessionRouter{
		notifyIdle: func() { notified++ },
	}
	// Compile-time: fake satisfies SessionRouter.
	var _ SessionRouter = fake

	d := &Dispatcher{router: fake}

	// Runtime: a routing call reaches the fake through the interface seam.
	// (GetSession was dropped from SessionRouter in #1587 once its only
	// production caller moved to the DiscardPassthroughPending seam, so we
	// exercise a method that remains on the interface.)
	d.router.NotifyIdle()
	if notified != 1 {
		t.Errorf("expected NotifyIdle to reach fake once, got %d", notified)
	}
}

// TestDispatcher_DiscardQueueRoutesThroughSeam proves discardQueue clears
// passthrough pending via the SessionRouter interface seam rather than
// dereferencing the concrete *session.ManagedSession behind GetSession
// (R20260602190132-ARCH-4, #1612). A fake router with no getSession closure
// would have panicked the old GetSession path only on a non-nil session; the
// seam means the fake observes the (key, reason) call directly.
func TestDispatcher_DiscardQueueRoutesThroughSeam(t *testing.T) {
	t.Parallel()

	var gotKey string
	var gotReason error
	called := 0
	fake := &fakeSessionRouter{
		discardPassthroughPending: func(key string, reason error) {
			called++
			gotKey = key
			gotReason = reason
		},
	}
	var _ SessionRouter = fake

	d := &Dispatcher{router: fake}
	d.discardQueue("im:direct:u1:general")

	if called != 1 {
		t.Fatalf("expected DiscardPassthroughPending called once via seam, got %d", called)
	}
	if gotKey != "im:direct:u1:general" {
		t.Errorf("key not forwarded through seam: got %q", gotKey)
	}
	if !errors.Is(gotReason, cli.ErrSessionReset) {
		t.Errorf("reason not forwarded through seam: got %v, want ErrSessionReset", gotReason)
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
	// AllowMissingSender: this test exercises only nil-router/discardQueue
	// behaviour and never reaches the IM Send path, so opt out of the
	// boot-panic check that was added in R248-ARCH-2.
	d, err := NewDispatcher(DispatcherConfig{Router: nil, AllowMissingSender: true})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	if d.router != nil {
		t.Fatal("Dispatcher.router should be untyped nil when cfg.Router is nil; typed-nil trap reintroduced")
	}
	// discardQueue with a nil router must be a no-op, not a panic.
	d.discardQueue("irrelevant:key:0:general")
}

// TestNewDispatcher_ResolverFabricatedWhenNil pins the contract that
// removed the legacy nil-resolver inline branches in dispatch.go /
// commands.go. Production wiring always passes a Resolver but headless
// constructions (and the in-tree test harnesses below) leave it nil; the
// constructor must fabricate a project-less fallback so that the IM,
// /urgent, and slash-command paths can dereference d.resolver
// unconditionally. If a future refactor drops the fabrication and
// reintroduces nil, the next IM message would crash with a nil pointer
// dereference instead of returning a sane unbound-chat key.
func TestNewDispatcher_ResolverFabricatedWhenNil(t *testing.T) {
	t.Parallel()

	// Case 1: no Resolver, no ProjectMgr — should still get a usable resolver.
	// AllowMissingSender: this case asserts on resolver / keyForChat only,
	// not the IM Send path; opt out of R248-ARCH-2 boot-panic.
	d, err := NewDispatcher(DispatcherConfig{
		Agents:             map[string]session.AgentOpts{"general": {}},
		AllowMissingSender: true,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	if d.resolver == nil {
		t.Fatal("NewDispatcher must fabricate a Resolver when cfg.Resolver is nil")
	}
	got := d.keyForChat("im", "direct", "user1", "general")
	if got != "im:direct:user1:general" {
		t.Errorf("unbound-chat key form drifted: got %q", got)
	}

	// Case 2: explicit Resolver passes through unchanged.
	custom := session.NewKeyResolver(map[string]session.AgentOpts{"general": {}}, nil)
	d2, err := NewDispatcher(DispatcherConfig{Resolver: custom, AllowMissingSender: true})
	if err != nil {
		t.Fatalf("NewDispatcher (custom resolver): %v", err)
	}
	if d2.resolver != custom {
		t.Fatal("explicit Resolver must be preserved, not replaced by a fresh fabrication")
	}
}

// TestNewDispatcher_PrefersRouterResolver covers R237-ARCH-12 (#604):
// when cfg.Resolver is unset and cfg.Router carries a Resolver via
// RouterConfig.Resolver, NewDispatcher must adopt the router's
// singleton instead of fabricating a parallel KeyResolver — that's
// the central remediation for agents-config drift across the 4
// historical construction sites.
func TestNewDispatcher_PrefersRouterResolver(t *testing.T) {
	t.Parallel()

	shared := session.NewKeyResolver(map[string]session.AgentOpts{"general": {}}, nil)
	router := session.NewRouter(session.RouterConfig{Resolver: shared})

	d, err := NewDispatcher(DispatcherConfig{
		Router:             router,
		Agents:             map[string]session.AgentOpts{"general": {}},
		AllowMissingSender: true,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	if d.resolver != shared {
		t.Fatalf("Dispatcher.resolver = %p, want router-shared %p — router resolver was not adopted", d.resolver, shared)
	}
}

// TestNewDispatcher_ResolverPrecedence pins the full 3-tier precedence
// chain documented for R237-ARCH-12 (#604): explicit cfg.Resolver always
// wins over the Router-attached singleton, and the fabricated fallback
// only fires when neither is wired. Without this regression pin, a future
// refactor could silently swap the order — explicit cfg.Resolver
// disrespected (the test override pathway) or the Router singleton
// re-fabricated (re-introducing drift) would each pass the existing
// single-case tests but break a documented invariant.
func TestNewDispatcher_ResolverPrecedence(t *testing.T) {
	t.Parallel()

	t.Run("explicit cfg.Resolver wins over router singleton", func(t *testing.T) {
		t.Parallel()
		routerOwned := session.NewKeyResolver(map[string]session.AgentOpts{"general": {}}, nil)
		explicit := session.NewKeyResolver(map[string]session.AgentOpts{"general": {}}, nil)
		if routerOwned == explicit {
			t.Fatal("test setup invariant: routerOwned and explicit must be distinct instances")
		}
		router := session.NewRouter(session.RouterConfig{Resolver: routerOwned})

		d, err := NewDispatcher(DispatcherConfig{
			Router:             router,
			Resolver:           explicit,
			Agents:             map[string]session.AgentOpts{"general": {}},
			AllowMissingSender: true,
		})
		if err != nil {
			t.Fatalf("NewDispatcher: %v", err)
		}
		if d.resolver != explicit {
			t.Fatalf("explicit cfg.Resolver lost to router singleton: got %p, want %p (router=%p)",
				d.resolver, explicit, routerOwned)
		}
	})

	t.Run("fabricated fallback only when both unwired", func(t *testing.T) {
		t.Parallel()
		// No cfg.Resolver, no cfg.Router → fabricated path.
		d, err := NewDispatcher(DispatcherConfig{
			Agents:             map[string]session.AgentOpts{"general": {}},
			AllowMissingSender: true,
		})
		if err != nil {
			t.Fatalf("NewDispatcher: %v", err)
		}
		if d.resolver == nil {
			t.Fatal("fabricated fallback returned nil resolver")
		}
	})

	t.Run("router without resolver still falls back to fabrication", func(t *testing.T) {
		t.Parallel()
		// Router that did NOT receive a Resolver in its config — the
		// dispatcher should still construct a fresh fallback rather
		// than panicking on the nil Router.Resolver() result.
		router := session.NewRouter(session.RouterConfig{})
		d, err := NewDispatcher(DispatcherConfig{
			Router:             router,
			Agents:             map[string]session.AgentOpts{"general": {}},
			AllowMissingSender: true,
		})
		if err != nil {
			t.Fatalf("NewDispatcher: %v", err)
		}
		if d.resolver == nil {
			t.Fatal("nil router resolver should fall through to fabrication, not leave d.resolver=nil")
		}
	})
}
