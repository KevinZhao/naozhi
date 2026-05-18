package node

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestNodeMeta_HasCap covers the three relevant cases for the routing
// gate consumers (selectNodeForBackend in package server) need:
//   - Empty cap query: always satisfied. Backends with nil/empty
//     RequiredNodeCaps (claude) iterate zero times; an "" string
//     should never reject. Defensive in case a future caller passes
//     "" through a slice element.
//   - Hit: cap was advertised at register time → true.
//   - Miss: cap was not advertised → false.
//   - Nil receiver: legacy / construction paths that don't allocate
//     a NodeMeta should still be safe to call HasCap on; we want a
//     false (deny) result rather than a panic. Mirrors the safety
//     guarantee documented on the method.
//
// Sprint 6b of docs/rfc/multi-backend.md.
func TestNodeMeta_HasCap(t *testing.T) {
	t.Parallel()

	full := &NodeMeta{
		NodeID:       "node-1",
		Capabilities: map[string]bool{"acp": true, "askuser": true},
		RegisteredAt: time.Now(),
	}
	empty := &NodeMeta{NodeID: "node-2"} // legacy: caps map nil

	cases := []struct {
		name string
		meta *NodeMeta
		cap  string
		want bool
	}{
		{"emptyCap_full", full, "", true},
		{"emptyCap_empty", empty, "", true},
		{"emptyCap_nil", nil, "", true},
		{"hit_acp", full, "acp", true},
		{"hit_askuser", full, "askuser", true},
		{"miss_unknownCap", full, "gemini", false},
		{"miss_emptyMap", empty, "acp", false},
		{"miss_nilReceiver", nil, "acp", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.meta.HasCap(tc.cap)
			if got != tc.want {
				t.Errorf("HasCap(%q) = %v; want %v", tc.cap, got, tc.want)
			}
		})
	}
}

// TestCapsFromSlice exercises the wire-format → lookup-format
// conversion that runs at register time. The contract:
//   - nil / empty slice → nil result (so ReverseMsg.Capabilities omitempty fires).
//   - duplicates collapse to a single key.
//   - empty strings are dropped (not stored as "" → true).
//   - all-empty input collapses to nil.
//
// Coverage is centralised here so the public selectNodeForBackend
// tests can rely on capsFromSlice without re-asserting its invariants.
func TestCapsFromSlice(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want map[string]bool
	}{
		{"nil", nil, nil},
		{"empty", []string{}, nil},
		{"single", []string{"acp"}, map[string]bool{"acp": true}},
		{"duplicate", []string{"acp", "acp"}, map[string]bool{"acp": true}},
		{"multi", []string{"acp", "gemini"}, map[string]bool{"acp": true, "gemini": true}},
		{"emptyEntry", []string{""}, nil},
		{"mixedEmpty", []string{"acp", "", "gemini"}, map[string]bool{"acp": true, "gemini": true}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := capsFromSlice(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (got %v)", len(got), len(tc.want), got)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("got[%q] = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

// TestNewReverseConnWithMeta_PopulatesCaps asserts the constructor wires
// the wire-form caps slice into NodeMeta.Capabilities. Routing at the
// server layer relies on this — without it Meta().HasCap("acp") would
// always return false even for a kiro-capable node.
//
// We pass a nil websocket.Conn — Meta() never touches it. We must not
// call Close() in this test for the same reason; the live-conn
// lifecycle has its own coverage in reverseconn_basectx_test.go.
func TestNewReverseConnWithMeta_PopulatesCaps(t *testing.T) {
	t.Parallel()

	rc := newReverseConnWithMeta("n1", "Node 1", "10.0.0.1", nil, []string{"acp", "askuser"}, "n1.host")

	meta := rc.Meta()
	if meta == nil {
		t.Fatal("Meta() returned nil")
	}
	if meta.NodeID != "n1" {
		t.Errorf("NodeID = %q, want n1", meta.NodeID)
	}
	if !meta.HasCap("acp") {
		t.Error("HasCap(acp) = false, want true")
	}
	if !meta.HasCap("askuser") {
		t.Error("HasCap(askuser) = false, want true")
	}
	if meta.HasCap("gemini") {
		t.Error("HasCap(gemini) = true, want false (not advertised)")
	}
	if meta.Hostname != "n1.host" {
		t.Errorf("Hostname = %q, want n1.host", meta.Hostname)
	}
	if meta.RegisteredAt.IsZero() {
		t.Error("RegisteredAt is zero — should be populated at construction")
	}
}

// TestReverseServer_Register_PopulatesMetaCaps is the round-trip
// integration test: connector-side caps → register frame on the wire
// → reverseserver.go ServeHTTP unmarshals → newReverseConnWithMeta
// stores them on NodeMeta.Capabilities. Without it the wiring at
// either end could regress silently (dropped Capabilities field on
// the ReverseMsg struct, missing assignment in the constructor) and
// every cap query would default-deny.
//
// We use the existing dialReverseNode + reverseAuth helpers but
// bypass reverseAuth (which doesn't accept caps) by writing the
// register frame directly. Captures the *ReverseConn out of
// OnRegister so we can read Meta() in the test goroutine.
func TestReverseServer_Register_PopulatesMetaCaps(t *testing.T) {
	rs := newTestReverseServer("node-acp", "tok", false)

	connCh := make(chan *ReverseConn, 1)
	var registered atomic.Bool
	rs.OnRegister = func(_ string, rc *ReverseConn) {
		registered.Store(true)
		select {
		case connCh <- rc:
		default:
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsConn := dialReverseNode(t, srv)
	defer wsConn.Close()

	// Register frame with caps — bypass reverseAuth so we can pass
	// the Capabilities field.
	if err := wsConn.WriteJSON(ReverseMsg{
		Type:         "register",
		NodeID:       "node-acp",
		Token:        "tok",
		Hostname:     "kiro-host",
		Capabilities: []string{"acp", "askuser"},
	}); err != nil {
		t.Fatalf("register write: %v", err)
	}
	wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var resp ReverseMsg
	if err := wsConn.ReadJSON(&resp); err != nil {
		t.Fatalf("register read: %v", err)
	}
	if resp.Type != "registered" {
		t.Fatalf("expected registered, got %q (err=%q)", resp.Type, resp.Error)
	}

	var rc *ReverseConn
	select {
	case rc = <-connCh:
	case <-time.After(2 * time.Second):
		t.Fatal("OnRegister was not called within 2s")
	}

	meta := rc.Meta()
	if meta == nil {
		t.Fatal("Meta() returned nil")
	}
	if !meta.HasCap("acp") {
		t.Error("HasCap(acp) = false; expected true after register frame")
	}
	if !meta.HasCap("askuser") {
		t.Error("HasCap(askuser) = false; expected true after register frame")
	}
	if meta.HasCap("gemini") {
		t.Error("HasCap(gemini) = true; not advertised so should be false")
	}
}

// TestNewReverseConn_LegacyEmptyCaps covers the legacy constructor:
// older code paths and tests still call newReverseConn directly. They
// must produce a connection whose Meta() reports an empty capability
// set so HasCap() denies any non-empty cap query (so kiro routing
// rejects them, claude routing accepts them).
//
// As above we skip Close to keep the test free of a live websocket.
func TestNewReverseConn_LegacyEmptyCaps(t *testing.T) {
	t.Parallel()
	rc := newReverseConn("legacy", "Legacy", "10.0.0.2", nil)

	meta := rc.Meta()
	if meta == nil {
		t.Fatal("Meta() returned nil")
	}
	if meta.HasCap("acp") {
		t.Error("legacy node HasCap(acp) = true, want false (no caps advertised)")
	}
	if !meta.HasCap("") {
		t.Error("HasCap(\"\") = false on legacy node, want true (empty cap is the no-op contract)")
	}
}
