package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clierr"
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
	d, err := NewDispatcher(DispatcherConfig{ProjectMgr: nil, Turns: testTurns()})
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
type fakeSessionRouter struct {
	interruptViaControl func(key string) session.InterruptOutcome
}

func (f *fakeSessionRouter) ResetChatAndSetWorkspace(string, string) {
	panic("fakeSessionRouter.ResetChatAndSetWorkspace not configured")
}

func (f *fakeSessionRouter) Workspace(string) string {
	panic("fakeSessionRouter.Workspace not configured")
}

func (f *fakeSessionRouter) SetSessionTuning(context.Context, string, *string, *string) (string, error) {
	panic("fakeSessionRouter.SetSessionTuning not configured")
}

func (f *fakeSessionRouter) SetSessionBackend(string, string) {
	panic("fakeSessionRouter.SetSessionBackend not configured")
}

func (f *fakeSessionRouter) VisitSessions(func(session.SessionSnapshot) bool) {
	panic("fakeSessionRouter.VisitSessions not configured")
}

func (f *fakeSessionRouter) InterruptSessionViaControl(key string) session.InterruptOutcome {
	if f.interruptViaControl == nil {
		panic("fakeSessionRouter.InterruptSessionViaControl not configured")
	}
	return f.interruptViaControl(key)
}

// TestDispatcher_AcceptsFakeSessionRouter is the smoke test that proves the
// consumer-interfaces refactor actually lets tests swap in a fake router: a
// routing call reaches the fake through the interface seam.
func TestDispatcher_AcceptsFakeSessionRouter(t *testing.T) {
	t.Parallel()

	var interrupted []string
	fake := &fakeSessionRouter{
		interruptViaControl: func(key string) session.InterruptOutcome {
			interrupted = append(interrupted, key)
			return session.InterruptSent
		},
	}
	var _ SessionRouter = fake

	d := &Dispatcher{router: fake}
	if got := d.router.InterruptSessionViaControl("im:direct:u1:general"); got != session.InterruptSent || len(interrupted) != 1 {
		t.Errorf("InterruptSessionViaControl through the seam = %v, calls %v; want Sent, one call", got, interrupted)
	}
}

// TestDispatcher_ResetRoutesThroughTurns proves /new reaches the session's
// in-flight passthrough sends and the session reset through Turns (the
// orchestrator's Sender), never the session itself (#1612): the Sender
// observes (key, ErrSessionReset) and then the reset of that key.
func TestDispatcher_ResetRoutesThroughTurns(t *testing.T) {
	t.Parallel()

	var calls []string
	sender := &testSender{
		discardPending: func(key string, reason error) {
			if !errors.Is(reason, clierr.ErrSessionReset) {
				t.Errorf("DiscardPending reason = %v, want ErrSessionReset", reason)
			}
			calls = append(calls, "discard "+key)
		},
		reset: func(key string, discardOverride bool) {
			calls = append(calls, fmt.Sprintf("reset %s %v", key, discardOverride))
		},
	}
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, withSender(sender))
	d.BuildHandler()(context.Background(), incomingMsg("/new"))

	want := []string{"discard fake:direct:chat1:general", "reset fake:direct:chat1:general false"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("Sender calls = %q, want %q (discard before reset, override kept)", calls, want)
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
	fake.Workspace("any")
}

// TestNewDispatcher_NilRouterStaysUntypedNil pins the typed-nil fix:
// when DispatcherConfig.Router is nil, or a nil *session.Router boxed into
// it, the Dispatcher.router interface field must hold untyped nil so
// `if d.router != nil` guards behave correctly.
func TestNewDispatcher_NilRouterStaysUntypedNil(t *testing.T) {
	t.Parallel()
	for _, r := range []SessionRouter{nil, (*session.Router)(nil)} {
		d, err := NewDispatcher(DispatcherConfig{Router: r, Turns: testTurns()})
		if err != nil {
			t.Fatalf("NewDispatcher: %v", err)
		}
		if d.router != nil {
			t.Fatalf("Dispatcher.router should be untyped nil when cfg.Router is %#v; typed-nil trap reintroduced", r)
		}
	}
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
	d, err := NewDispatcher(DispatcherConfig{
		Agents: map[string]session.AgentOpts{"general": {}},
		Turns:  testTurns(),
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
	d2, err := NewDispatcher(DispatcherConfig{Resolver: custom, Turns: testTurns()})
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
		Router: routerOf(router),
		Agents: map[string]session.AgentOpts{"general": {}},
		Turns:  testTurns(),
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
			Router:   routerOf(router),
			Resolver: explicit,
			Agents:   map[string]session.AgentOpts{"general": {}},
			Turns:    testTurns(),
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
			Agents: map[string]session.AgentOpts{"general": {}},
			Turns:  testTurns(),
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
			Router: routerOf(router),
			Agents: map[string]session.AgentOpts{"general": {}},
			Turns:  testTurns(),
		})
		if err != nil {
			t.Fatalf("NewDispatcher: %v", err)
		}
		if d.resolver == nil {
			t.Fatal("nil router resolver should fall through to fabrication, not leave d.resolver=nil")
		}
	})
}
