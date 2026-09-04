package main

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func scanFixture(t *testing.T, src string) []Violation {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "routes.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return scanAPIRouteOwnerFile(fset, f)
}

// TestAPIRouteOwner_FlagsServerMethodOnAPIRoute covers the shapes rule 6 must
// catch: bare, auth-wrapped, and doubly wrapped Server handlers on /api/*.
func TestAPIRouteOwner_FlagsServerMethodOnAPIRoute(t *testing.T) {
	src := `package server
func (s *Server) registerDashboard() {
	auth := s.auth.RequireAuth
	s.mux.HandleFunc("GET /api/system/daemons", auth(s.handleSystemDaemons))
	s.mux.HandleFunc("POST /api/system/update/apply", s.handleUpdateApply)
	s.mux.Handle("GET /api/planner/stats", auth(wrap(s.handlePlannerStats)))
	s.mux.HandleFunc("GET /api/foo", auth(s.HandleFoo))
}`
	vs := scanFixture(t, src)
	if len(vs) != 4 {
		t.Fatalf("got %d violations, want 4:\n%v", len(vs), vs)
	}
	want := []string{"Server.handleSystemDaemons", "Server.handleUpdateApply", "Server.handlePlannerStats", "Server.HandleFoo"}
	for i, v := range vs {
		if v.Rule != "api_route_owner" {
			t.Errorf("[%d] rule = %q, want api_route_owner", i, v.Rule)
		}
		if !strings.Contains(v.Message, want[i]) {
			t.Errorf("[%d] message %q does not name %q", i, v.Message, want[i])
		}
		if !strings.Contains(v.Message, "internal/server/doc.go") {
			t.Errorf("[%d] message %q must point at internal/server/doc.go", i, v.Message)
		}
		if v.Line == 0 {
			t.Errorf("[%d] line = 0, want the registration line", i)
		}
	}
	// Lines are 4..7 in the fixture, in registration order.
	for i, v := range vs {
		if v.Line != 4+i {
			t.Errorf("[%d] line = %d, want %d", i, v.Line, 4+i)
		}
	}
}

// TestAPIRouteOwner_AllowsDashboardAndPipeHandlers covers what the rule must
// not report: dashboard sub-package handlers (selector through a field),
// non-/api routes on Server methods, package-level funcs, and non-mux calls.
func TestAPIRouteOwner_AllowsDashboardAndPipeHandlers(t *testing.T) {
	src := `package server
func (s *Server) registerDashboard() {
	auth := s.auth.RequireAuth
	s.mux.HandleFunc("GET /api/system/daemons", auth(s.systemH.HandleDaemons))
	s.mux.HandleFunc("GET /api/sessions", auth(s.sessionH.HandleList))
	s.mux.HandleFunc("POST /api/auth/login", s.auth.HandleLogin)
	s.mux.HandleFunc("GET /dashboard", s.handleDashboard)
	s.mux.HandleFunc("GET /static/dashboard.js", auth(handleDashboardJS))
	s.mux.HandleFunc("GET /ws", s.hub.HandleUpgrade)
	s.mux.Handle("GET /ws-node", s.reverseNodeServer)
	s.mux.HandleFunc("GET /api/debug/vars", s.auth.RequireAuth(handler))
	other.mux.HandleFunc("GET /api/x", other.handleX)
	s.HandleFunc("GET /api/y", s.handleY)
}
func (h *Hub) register() {
	h.mux.HandleFunc("GET /api/z", h.handleZ)
}
func handleDashboardJS(w http.ResponseWriter, r *http.Request) {}`
	if vs := scanFixture(t, src); len(vs) != 0 {
		t.Fatalf("got %d violations, want 0:\n%v", len(vs), vs)
	}
}

// TestAPIRouteOwner_FollowsReceiverName pins that the receiver identifier is
// read from the method, not assumed to be `s`.
func TestAPIRouteOwner_FollowsReceiverName(t *testing.T) {
	src := `package server
func (srv *Server) registerAPI() {
	srv.mux.HandleFunc("GET /api/x", auth(srv.handleX))
}`
	vs := scanFixture(t, src)
	if len(vs) != 1 || !strings.Contains(vs[0].Message, "Server.handleX") {
		t.Fatalf("got %v, want one violation naming Server.handleX", vs)
	}
}

// TestAPIRouteOwner_FollowsMethodValueLocals pins that a Server handler bound
// to a local first (`h := s.handleX`, `var h = s.handleX`, reassignment) is
// still attributed when the local is registered, while an unrelated local is
// not.
func TestAPIRouteOwner_FollowsMethodValueLocals(t *testing.T) {
	src := `package server
func (s *Server) registerAPI() {
	h := s.handleFoo
	var g = s.HandleBar
	var k http.HandlerFunc
	k = s.handleBaz
	other := s.systemH.HandleDaemons
	s.mux.HandleFunc("GET /api/foo", auth(h))
	s.mux.HandleFunc("GET /api/bar", g)
	s.mux.Handle("GET /api/baz", auth(wrap(k)))
	s.mux.HandleFunc("GET /api/other", auth(other))
	s.mux.HandleFunc("GET /dashboard", h)
}`
	vs := scanFixture(t, src)
	want := []string{"Server.handleFoo", "Server.HandleBar", "Server.handleBaz"}
	if len(vs) != len(want) {
		t.Fatalf("got %d violations, want %d:\n%v", len(vs), len(want), vs)
	}
	for i, v := range vs {
		if !strings.Contains(v.Message, want[i]) {
			t.Errorf("[%d] message %q does not name %q", i, v.Message, want[i])
		}
	}
}

// TestAPIRouteOwner_LiveRoutesClean runs rule 6 against the real routes.go
// so the repository state cannot drift from the rule while its unit fixtures
// stay green.
func TestAPIRouteOwner_LiveRoutesClean(t *testing.T) {
	vs, err := scanAPIRouteOwner("../../internal/server/routes.go")
	if err != nil {
		t.Fatalf("scanAPIRouteOwner: %v", err)
	}
	if len(vs) != 0 {
		t.Errorf("routes.go has %d /api route(s) on Server methods:\n%v", len(vs), vs)
	}
}
