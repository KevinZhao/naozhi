package main

import (
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSiblingPkg(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// parseSiblingPkg writes files as a fixture package and parses it through
// parseSiblingPkgDir — the same helper scanSendEngineSibling uses — rather
// than calling go/parser itself: a _test.go file that reads disk and names a
// .go file is exactly what internal/testhelper's source-anchor ratchet counts
// (#2716), and every check here already has a non-test entry point to call.
func parseSiblingPkg(t *testing.T, files map[string]string) (*token.FileSet, []siblingSrcFile) {
	t.Helper()
	dir := writeSiblingPkg(t, files)
	fset, out, err := parseSiblingPkgDir(dir)
	if err != nil {
		t.Fatalf("parseSiblingPkgDir: %v", err)
	}
	return fset, out
}

// siblingTodayPkg mirrors master as of #2897 S5a: NewHub constructs its own engine
// and reaches it through *Hub, Hub still declares all four notifier methods,
// and LegacySendInvokes reads a field off the engine directly. This is the
// shape siblingCtorBaseline/siblingEngineReachBaseline/
// siblingHubNotifierBaseline/siblingFieldReadBaseline are set to.
func siblingTodayPkg() map[string]string {
	return map[string]string{
		"wshub.go": `package server

type Hub struct {
	engine *sendEngine
}

func NewHub(opts HubOptions) *Hub {
	h := &Hub{}
	h.engine = newSendEngine(sendEngineOpts{})
	return h
}

func (h *Hub) broadcastState()         {}
func (h *Hub) BroadcastSessionReady()  {}
func (h *Hub) BroadcastSessionsUpdate() {}
func (h *Hub) broadcastSendError()     {}

func (h *Hub) LegacySendInvokes() int64 {
	return h.engine.legacyInvokes
}
`,
		"build_dashboard.go": `package server

func (s *Server) buildDashboard(hs *handlerSet) {
	_ = s.hub.engine
}
`,
		"send.go": `package server

func (s *Server) sendWithBroadcast() {
	s.hub.engine.sendWithBroadcast()
}
`,
	}
}

// siblingCleanPkg is the S5c2 target shape: buildWSStack is the sole constructor and
// sole caller boundary, Hub holds no notifier methods, and nothing reaches a
// field directly. Every check should report zero against it.
func siblingCleanPkg() map[string]string {
	return map[string]string{
		"wshub.go": `package server

type Hub struct {
	engine *sendEngine
}

type HubOptions struct {
	Engine *sendEngine
}

func NewHub(opts HubOptions) *Hub {
	return &Hub{engine: opts.Engine}
}

func (h *Hub) Shutdown() {
	h.engine.drain()
}
`,
		"build_dashboard.go": `package server

func (s *Server) buildWSStack(w *wiring) *Hub {
	e := newSendEngine(sendEngineOpts{})
	w.engine = e
	return NewHub(HubOptions{Engine: e})
}

func (s *Server) buildDashboard(hs *handlerSet) {
	s.hub = s.buildWSStack(hs.wiring)
}
`,
		"send_engine.go": `package server

type sendEngine struct {
	notify sendNotifier
}

func newSendEngine(o sendEngineOpts) *sendEngine {
	return &sendEngine{}
}

func (e *sendEngine) drain() {}
`,
		"handler_set.go": `package server

type wiring struct {
	engine *sendEngine
}
`,
		"send.go": `package server

type serverCaps struct {
	s    *Server
	send *sendEngine
}
`,
	}
}

func withExtra(base map[string]string, name, src string) map[string]string {
	out := make(map[string]string, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out[name] = src
	return out
}

// --- C1: construction point ---

func TestScanSiblingCtorPoints(t *testing.T) {
	t.Parallel()
	fset, files := parseSiblingPkg(t, siblingTodayPkg())
	if vs := scanSiblingCtorPoints(fset, files); len(vs) != 1 {
		t.Fatalf("today: want 1 (NewHub's own newSendEngine call), got %d: %+v", len(vs), vs)
	}

	fset, files = parseSiblingPkg(t, siblingCleanPkg())
	if vs := scanSiblingCtorPoints(fset, files); len(vs) != 0 {
		t.Fatalf("clean: want 0, got %d: %+v", len(vs), vs)
	}

	// m5: a helper wraps newSendEngine. One more disallowed call site.
	withHelper := withExtra(siblingTodayPkg(), "helper_extra.go", `package server

func wrapNewSendEngine() *sendEngine {
	return newSendEngine(sendEngineOpts{})
}
`)
	fset, files = parseSiblingPkg(t, withHelper)
	if vs := scanSiblingCtorPoints(fset, files); len(vs) != 2 {
		t.Fatalf("m5 helper wraps newSendEngine: want 2, got %d: %+v", len(vs), vs)
	}

	// m6: buildWSStack called from outside buildDashboard.
	withRogueCaller := withExtra(siblingCleanPkg(), "rogue_extra.go", `package server

func rogueCaller(s *Server, w *wiring) {
	s.buildWSStack(w)
}
`)
	fset, files = parseSiblingPkg(t, withRogueCaller)
	if vs := scanSiblingCtorPoints(fset, files); len(vs) != 1 {
		t.Fatalf("m6 buildWSStack called outside buildDashboard: want 1, got %d: %+v", len(vs), vs)
	}
}

// --- C2: engine reach points ---

func TestScanSiblingEngineReach(t *testing.T) {
	t.Parallel()
	fset, files := parseSiblingPkg(t, siblingTodayPkg())
	if vs := scanSiblingEngineReach(fset, files); len(vs) != 3 {
		t.Fatalf("today: want 3 (build_dashboard.go, send.go, NewHub's h.engine=), got %d: %+v", len(vs), vs)
	}

	fset, files = parseSiblingPkg(t, siblingCleanPkg())
	if vs := scanSiblingEngineReach(fset, files); len(vs) != 0 {
		t.Fatalf("clean: want 0, got %d: %+v", len(vs), vs)
	}

	// m2: alias read. `hub := s.hub; hub.engine` is not a *Hub/*SendHandler
	// receiver, a `w *wiring` parameter, or `hs.wiring`.
	withAlias := withExtra(siblingCleanPkg(), "alias_extra.go", `package server

func (s *Server) peek() *sendEngine {
	hub := s.hub
	return hub.engine
}
`)
	fset, files = parseSiblingPkg(t, withAlias)
	if vs := scanSiblingEngineReach(fset, files); len(vs) != 1 {
		t.Fatalf("m2 alias read: want 1, got %d: %+v", len(vs), vs)
	}

	// The two allowed shapes must NOT be flagged: a `w *wiring` parameter and
	// `hs.wiring`.
	allowedShapes := withExtra(siblingCleanPkg(), "allowed_extra.go", `package server

func readViaWiring(w *wiring) *sendEngine {
	return w.engine
}

func readViaHandlerSet(hs *handlerSet) *sendEngine {
	return hs.wiring.engine
}
`)
	fset, files = parseSiblingPkg(t, allowedShapes)
	if vs := scanSiblingEngineReach(fset, files); len(vs) != 0 {
		t.Fatalf("allowed shapes (w *wiring, hs.wiring) must not be flagged: %+v", vs)
	}
}

// --- C3: accessor ---

func TestScanSiblingAccessors(t *testing.T) {
	t.Parallel()
	fset, files := parseSiblingPkg(t, siblingCleanPkg())
	if vs := scanSiblingAccessors(fset, files); len(vs) != 0 {
		t.Fatalf("clean: want 0, got %d: %+v", len(vs), vs)
	}
	// newSendEngine itself returns *sendEngine and must not be flagged.
	fset, files = parseSiblingPkg(t, siblingTodayPkg())
	if vs := scanSiblingAccessors(fset, files); len(vs) != 0 {
		t.Fatalf("today (newSendEngine itself returns *sendEngine): want 0, got %d: %+v", len(vs), vs)
	}

	// m1: an accessor.
	withAccessor := withExtra(siblingCleanPkg(), "accessor_extra.go", `package server

func (h *Hub) Engine() *sendEngine {
	return h.engine
}
`)
	fset, files = parseSiblingPkg(t, withAccessor)
	if vs := scanSiblingAccessors(fset, files); len(vs) != 1 {
		t.Fatalf("m1 accessor: want 1, got %d: %+v", len(vs), vs)
	}

	// A closure accessor must also be caught.
	withClosure := withExtra(siblingCleanPkg(), "closure_extra.go", `package server

func makeAccessor(h *Hub) func() *sendEngine {
	return func() *sendEngine {
		return h.engine
	}
}
`)
	fset, files = parseSiblingPkg(t, withClosure)
	if vs := scanSiblingAccessors(fset, files); len(vs) != 1 {
		t.Fatalf("closure accessor: want 1, got %d: %+v", len(vs), vs)
	}
}

// --- C4: holder whitelist ---

func TestScanSiblingHolderWhitelist(t *testing.T) {
	t.Parallel()
	fset, files := parseSiblingPkg(t, siblingCleanPkg())
	if vs := scanSiblingHolderWhitelist(fset, files); len(vs) != 0 {
		t.Fatalf("clean (Hub.engine, HubOptions.Engine, wiring.engine, serverCaps.send all whitelisted): want 0, got %d: %+v", len(vs), vs)
	}

	// m7: a new struct holds *sendEngine.
	withBox := withExtra(siblingCleanPkg(), "box_extra.go", `package server

type engineBox struct {
	e *sendEngine
}
`)
	fset, files = parseSiblingPkg(t, withBox)
	if vs := scanSiblingHolderWhitelist(fset, files); len(vs) != 1 {
		t.Fatalf("m7 engineBox: want 1, got %d: %+v", len(vs), vs)
	}
}

// --- C5: Hub is not a notifier ---

func TestScanSiblingHubNotifiers(t *testing.T) {
	t.Parallel()
	fset, files := parseSiblingPkg(t, siblingTodayPkg())
	if vs := scanSiblingHubNotifiers(fset, files); len(vs) != 4 {
		t.Fatalf("today: want 4 (broadcastState, BroadcastSessionReady, BroadcastSessionsUpdate, broadcastSendError), got %d: %+v", len(vs), vs)
	}

	fset, files = parseSiblingPkg(t, siblingCleanPkg())
	if vs := scanSiblingHubNotifiers(fset, files); len(vs) != 0 {
		t.Fatalf("clean: want 0, got %d: %+v", len(vs), vs)
	}
}

// --- C6: notifier does not point back ---

func TestScanSiblingNotifierBackpointer(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"today", "clean"} {
		pkg := siblingTodayPkg()
		if name == "clean" {
			pkg = siblingCleanPkg()
		}
		fset, files := parseSiblingPkg(t, pkg)
		if vs := scanSiblingNotifierBackpointer(fset, files); len(vs) != 0 {
			t.Fatalf("%s: want 0 (Hub itself has no Hub field), got %d: %+v", name, len(vs), vs)
		}
	}

	// m3: wsBroadcaster declares broadcastState and holds a *Hub back-pointer.
	withBcastBackpointer := withExtra(siblingCleanPkg(), "bcast_extra.go", `package server

type wsBroadcaster struct {
	hub *Hub
}

func (b *wsBroadcaster) broadcastState() {}
`)
	fset, files := parseSiblingPkg(t, withBcastBackpointer)
	if vs := scanSiblingNotifierBackpointer(fset, files); len(vs) != 1 {
		t.Fatalf("m3 wsBroadcaster.hub *Hub: want 1, got %d: %+v", len(vs), vs)
	}

	// m4: a hubNotifier adapter implements broadcastSendError via a Hub field.
	withAdapter := withExtra(siblingCleanPkg(), "adapter_extra.go", `package server

type hubNotifier struct {
	h *Hub
}

func (n *hubNotifier) broadcastSendError() {}
`)
	fset, files = parseSiblingPkg(t, withAdapter)
	if vs := scanSiblingNotifierBackpointer(fset, files); len(vs) != 1 {
		t.Fatalf("m4 hubNotifier.h *Hub: want 1, got %d: %+v", len(vs), vs)
	}
}

// --- C7: no field reads ---

func TestScanSiblingFieldReads(t *testing.T) {
	t.Parallel()
	fset, files := parseSiblingPkg(t, siblingTodayPkg())
	if vs := scanSiblingFieldReads(fset, files); len(vs) != 1 {
		t.Fatalf("today (LegacySendInvokes reads h.engine.legacyInvokes): want 1, got %d: %+v", len(vs), vs)
	}

	fset, files = parseSiblingPkg(t, siblingCleanPkg())
	if vs := scanSiblingFieldReads(fset, files); len(vs) != 0 {
		t.Fatalf("clean: want 0, got %d: %+v", len(vs), vs)
	}

	// m8: a *Hub method reads h.engine.queue directly instead of calling a
	// method.
	withFieldRead := withExtra(siblingCleanPkg(), "fieldread_extra.go", `package server

func (h *Hub) peekQueue() interface{} {
	return h.engine.queue
}
`)
	fset, files = parseSiblingPkg(t, withFieldRead)
	if vs := scanSiblingFieldReads(fset, files); len(vs) != 1 {
		t.Fatalf("m8 h.engine.queue: want 1, got %d: %+v", len(vs), vs)
	}

	// A method call through engine/bcast must not be flagged.
	withCallOnly := withExtra(siblingCleanPkg(), "callonly_extra.go", `package server

func (h *Hub) touch() {
	h.engine.TrackSend()
}
`)
	fset, files = parseSiblingPkg(t, withCallOnly)
	if vs := scanSiblingFieldReads(fset, files); len(vs) != 0 {
		t.Fatalf("method call must not be flagged: %+v", vs)
	}
}

// --- C8: construction does not reach into Hub ---

func TestScanSiblingBuildStepBcast(t *testing.T) {
	t.Parallel()
	fset, files := parseSiblingPkg(t, siblingCleanPkg())
	if vs := scanSiblingBuildStepBcast(fset, files); len(vs) != 0 {
		t.Fatalf("clean: want 0, got %d: %+v", len(vs), vs)
	}

	// m9: a build step reads s.hub.bcast directly.
	withBuildBcast := withExtra(siblingCleanPkg(), "buildbcast_extra.go", `package server

func buildTelemetryBinding(s *Server) {
	_ = s.hub.bcast
}
`)
	fset, files = parseSiblingPkg(t, withBuildBcast)
	if vs := scanSiblingBuildStepBcast(fset, files); len(vs) != 1 {
		t.Fatalf("m9 build step reads s.hub.bcast: want 1, got %d: %+v", len(vs), vs)
	}

	// A non-build-step function must not be flagged for the same pattern.
	withNonBuildBcast := withExtra(siblingCleanPkg(), "nonbuild_extra.go", `package server

func attachReverseNodeServer(s *Server) {
	_ = s.hub.bcast
}
`)
	fset, files = parseSiblingPkg(t, withNonBuildBcast)
	if vs := scanSiblingBuildStepBcast(fset, files); len(vs) != 0 {
		t.Fatalf("non-build-step reading s.hub.bcast is allowed by C8 (runtime path, owner decision 2): %+v", vs)
	}
}

// --- ratchetViolation: both directions, independent of any live baseline ---

func TestRatchetViolation(t *testing.T) {
	t.Parallel()
	one := []Violation{{Rule: "send_engine_sibling", File: "f.go", Line: 1}}
	two := []Violation{{Rule: "send_engine_sibling", File: "f.go", Line: 1}, {Rule: "send_engine_sibling", File: "f.go", Line: 2}}

	if vs := ratchetViolation("send_engine_sibling", "xBaseline", 1, one, "pkg"); len(vs) != 0 {
		t.Errorf("at baseline: want 0, got %+v", vs)
	}
	if vs := ratchetViolation("send_engine_sibling", "xBaseline", 1, two, "pkg"); len(vs) != 1 || !strings.Contains(vs[0].Message, "above the baseline") {
		t.Errorf("one over: want 1 'above the baseline', got %+v", vs)
	}
	if vs := ratchetViolation("send_engine_sibling", "xBaseline", 2, one, "pkg"); len(vs) != 1 || !strings.Contains(vs[0].Message, "lower xBaseline to 1") {
		t.Errorf("one under: want 1 'lower xBaseline to 1', got %+v", vs)
	}
}

// scanSendEngineSibling is the entry point lint-server-handlers actually
// calls: a smoke test against the real package pins that it is wired up and
// agrees with the live baselines (the per-check unit tests above do not
// exercise main.go's wiring or the real source tree).
func TestScanSendEngineSibling_RealPackage(t *testing.T) {
	t.Parallel()
	if vs := scanSendEngineSibling("../../internal/server"); len(vs) != 0 {
		t.Errorf("internal/server should sit exactly at today's baselines: %+v", vs)
	}
}
