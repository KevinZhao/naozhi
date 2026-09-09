package server

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// startBody returns the source of Server.Start, scoped to the function.
func startBody(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(self), "server.go"))
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	body := string(raw)
	const decl = "func (s *Server) Start(ctx context.Context) error {"
	i := strings.Index(body, decl)
	if i < 0 {
		t.Fatal("server.go: Server.Start not found")
	}
	start := body[i:]
	if j := strings.Index(start[len(decl):], "\nfunc "); j >= 0 {
		start = start[:len(decl)+j]
	}
	return start
}

// TestServerStart_ConstructsNoDependencies pins #2633 (E2 #2552 acceptance
// item 2, which the closing comment marked done while Start still built the
// Dispatcher and back-filled healthH.dispatcherMetrics). Everything the
// Server depends on is built in buildServerWithHandlers; Start turns the
// dispatcher into a handler, registers/starts platforms, and serves. The only
// values it may construct are the listener and the *http.Server itself.
//
// Source-level because the failure mode is structural: a `New*(` call in
// Start compiles and works, it just reopens the "field exists at
// construction, value arrives at Start" window that #431 / #2552 closed.
func TestServerStart_ConstructsNoDependencies(t *testing.T) {
	t.Parallel()
	start := startBody(t)

	// Positive anchor: Start still does the two things it is allowed to.
	if !strings.Contains(start, "s.dispatcher.BuildHandler()") {
		t.Error("Start must obtain the platform handler from the pre-built s.dispatcher")
	}
	if !strings.Contains(start, "listenTCP(") || !strings.Contains(start, "&http.Server{") {
		t.Error("Start must still bind the listener and build the *http.Server — anchor drift")
	}

	// Negative: no constructor calls other than the listener / http.Server.
	ctor := regexp.MustCompile(`\b(?:[A-Za-z_]\w*\.)?New[A-Z]\w*\(`)
	for _, m := range ctor.FindAllString(start, -1) {
		t.Errorf("Start constructs %s — build it in buildServerWithHandlers and hand Start the finished value (#2633)", m)
	}
	// The specific regression: the dispatcher or the health metrics binding
	// coming back into Start under any spelling.
	for _, needle := range []string{"dispatch.NewDispatcher", "dispatcherMetrics ="} {
		if strings.Contains(start, needle) {
			t.Errorf("Start contains %q — the dispatcher is constructed before HealthHandler in buildServerWithHandlers (#2633)", needle)
		}
	}
}

// TestServerStart_AppCancelDeferredBeforeFirstReturn pins the other half of
// #2633: `defer s.appCancel()` must precede every `return` in Start, so an
// early error (platform Start failure, listen failure) cancels the appCtx
// tree instead of leaving Hub / loops / dispatcher StopCtx alive.
// shutdown_complete_early_return_test.go checks the behaviour on one path;
// this pins the shape for all of them.
func TestServerStart_AppCancelDeferredBeforeFirstReturn(t *testing.T) {
	t.Parallel()
	start := startBody(t)

	deferIdx := strings.Index(start, "defer s.appCancel()")
	if deferIdx < 0 {
		t.Fatal("Start no longer defers s.appCancel()")
	}
	firstReturn := regexp.MustCompile(`(?m)^\s*return\b`).FindStringIndex(start)
	if firstReturn == nil {
		t.Fatal("Start has no return statement — anchor drift")
	}
	if firstReturn[0] < deferIdx {
		t.Errorf("Start has a return (offset %d) before `defer s.appCancel()` (offset %d): that path leaves appCtx live (#2633)", firstReturn[0], deferIdx)
	}
}
