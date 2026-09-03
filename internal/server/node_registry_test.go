package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/node"
)

// TestLookupNode_JSONErrorEnvelope pins #451 / R247-ARCH-3: LookupNode's
// three rejection paths (too-long / invalid-charset / unknown) now reply
// with the unified errEnvelope JSON shape carrying a stable machine code,
// instead of text/plain http.Error. Every caller is a dashboard JSON API
// handler whose front-end reads body.error, so a plain-text reply forced
// the UI to branch on Content-Type.
func TestLookupNode_JSONErrorEnvelope(t *testing.T) {
	t.Parallel()

	acc := newNodeRegistry(nil)

	cases := []struct {
		name     string
		id       string
		wantCode string
	}{
		{"too long", strings.Repeat("a", maxNodeIDBytes+1), "node_id_too_long"},
		{"invalid charset", "bad id!", "node_id_invalid"},
		{"unknown node", "ghost", "node_unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			nc, ok := acc.LookupNode(w, tc.id)
			if ok || nc != nil {
				t.Fatalf("LookupNode(%q) should reject, got ok=%v nc=%v", tc.id, ok, nc)
			}
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var env errEnvelope
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("body is not JSON: %v (body=%q)", err, w.Body.String())
			}
			if env.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", env.Code, tc.wantCode)
			}
			if env.Error == "" {
				t.Error("error message should be non-empty")
			}
		})
	}
}

// TestLookupNode_HitWritesNothing is the happy-path complement: a registered
// id returns the conn and leaves the ResponseWriter untouched so the caller
// can go on to write its own reply.
func TestLookupNode_HitWritesNothing(t *testing.T) {
	t.Parallel()

	stub := &fakeCapNode{id: "node-a"}
	reg := newNodeRegistry(map[string]node.Conn{"node-a": stub})
	w := httptest.NewRecorder()
	nc, ok := reg.LookupNode(w, "node-a")
	if !ok || nc != stub {
		t.Fatalf("LookupNode(node-a) = (%v, %v), want (stub, true)", nc, ok)
	}
	if w.Body.Len() != 0 || len(w.Header()) != 0 {
		t.Errorf("LookupNode hit wrote to the response: code=%d body=%q headers=%v", w.Code, w.Body.String(), w.Header())
	}
}

// TestNodeRegistry_NilSeed: the ctor must normalise a nil config table to
// an empty, fully usable registry (bare test servers / single-node
// deployments pass nil), and every read must be nil-safe on it.
func TestNodeRegistry_NilSeed(t *testing.T) {
	t.Parallel()

	reg := newNodeRegistry(nil)
	if reg.Len() != 0 || reg.HasNodes() {
		t.Fatalf("empty registry: Len=%d HasNodes=%v", reg.Len(), reg.HasNodes())
	}
	if snap := reg.NodesSnapshot(); snap == nil || len(snap) != 0 {
		t.Errorf("NodesSnapshot on empty = %v, want non-nil empty map", snap)
	}
	if conns := reg.Conns(); conns != nil {
		t.Errorf("Conns on empty = %v, want nil", conns)
	}
	if known := reg.KnownNodes(); known == nil || len(known) != 0 {
		t.Errorf("KnownNodes on empty = %v, want non-nil empty map", known)
	}
	if st := reg.NodesStatus(); len(st) != 0 {
		t.Errorf("NodesStatus on empty = %v, want empty", st)
	}
	if _, ok := reg.NodeByID("x"); ok {
		t.Error("NodeByID on empty registry hit")
	}
	reg.Remove("absent") // must be a no-op, not a panic
}

// TestNodeRegistry_SeedPromotesKnownButAddDoesNot mirrors the pre-G2 ctor
// semantics: configured nodes are both live and known (display name from
// the conn), whereas a runtime Add (reverse-node OnRegister) only touches
// the live table — known display names come from config via SetKnown.
func TestNodeRegistry_SeedPromotesKnownButAddDoesNot(t *testing.T) {
	t.Parallel()

	seeded := &fakeCapNode{id: "cfg"}
	reg := newNodeRegistry(map[string]node.Conn{"cfg": seeded})
	if got := reg.KnownNodes()["cfg"]; got != seeded.DisplayName() {
		t.Errorf("seeded known[cfg] = %q, want DisplayName %q", got, seeded.DisplayName())
	}

	reg.Add("dyn", &fakeCapNode{id: "dyn"})
	if _, known := reg.KnownNodes()["dyn"]; known {
		t.Error("Add promoted a runtime node into KnownNodes; only SetKnown may do that")
	}
	if nc, ok := reg.NodeByID("dyn"); !ok || nc == nil {
		t.Error("Add did not make the node visible to NodeByID")
	}

	// NodesStatus reports every known node; live ones carry the conn's
	// Status(), configured-but-offline ones read "disconnected", and
	// unknown live ones (dyn) are not listed — same as the pre-G2 /health.
	reg.SetKnown("offline", "Offline Box")
	got := reg.NodesStatus()
	want := map[string]string{"cfg": "ok", "offline": "disconnected"}
	if len(got) != len(want) {
		t.Fatalf("NodesStatus = %v, want %v", got, want)
	}
	for id, st := range want {
		if got[id] != st {
			t.Errorf("NodesStatus[%s] = %q, want %q", id, got[id], st)
		}
	}
}

// TestNodeRegistry_SnapshotIsDetached: mutating a NodesSnapshot must not
// leak into the registry (nodeCache holds snapshots across refreshes).
func TestNodeRegistry_SnapshotIsDetached(t *testing.T) {
	t.Parallel()

	reg := newNodeRegistry(map[string]node.Conn{"a": &fakeCapNode{id: "a"}})
	snap := reg.NodesSnapshot()
	delete(snap, "a")
	snap["b"] = &fakeCapNode{id: "b"}
	if _, ok := reg.NodeByID("a"); !ok {
		t.Error("deleting from the snapshot removed the node from the registry")
	}
	if _, ok := reg.NodeByID("b"); ok {
		t.Error("inserting into the snapshot added a node to the registry")
	}
	reg.Remove("a")
	if reg.HasNodes() {
		t.Error("Remove left HasNodes true")
	}
}

// TestNodeRegistry_AppendConnsAllocContract pins the single-RLock snapshot
// seam Hub.unregister depends on: alloc is called exactly once with the live
// count, its buffer is the one filled, and an empty table never calls alloc.
func TestNodeRegistry_AppendConnsAllocContract(t *testing.T) {
	t.Parallel()

	reg := newNodeRegistry(nil)
	calls := 0
	if out := reg.appendConns(func(int) []node.Conn { calls++; return nil }); out != nil || calls != 0 {
		t.Fatalf("empty appendConns: out=%v calls=%d, want nil/0", out, calls)
	}

	const n = 3
	for i := 0; i < n; i++ {
		reg.Add(fmt.Sprintf("n%d", i), &fakeCapNode{id: fmt.Sprintf("n%d", i)})
	}
	buf := make([]node.Conn, 0, 8)
	var gotN int
	out := reg.appendConns(func(k int) []node.Conn { calls++; gotN = k; return buf })
	if calls != 1 || gotN != n {
		t.Errorf("alloc calls=%d n=%d, want 1/%d", calls, gotN, n)
	}
	if len(out) != n {
		t.Errorf("appended %d conns, want %d", len(out), n)
	}
	if cap(out) != cap(buf) || &out[:1][0] != &buf[:1][0] {
		t.Error("appendConns did not fill the caller-provided buffer (pool reuse would be defeated)")
	}
	seen := map[string]bool{}
	for _, c := range out {
		seen[c.NodeID()] = true
	}
	if len(seen) != n {
		t.Errorf("appendConns yielded duplicates or misses: %v", seen)
	}
	if conns := reg.Conns(); len(conns) != n {
		t.Errorf("Conns len = %d, want %d", len(conns), n)
	}
}

// TestNodeRegistry_ConcurrentAccess hammers every method from many goroutines
// so `go test -race` proves the registry — not caller discipline — provides
// the mutual exclusion #2192 asked for. Assertions are deliberately weak
// (no panics, consistent final state); the race detector is the oracle.
func TestNodeRegistry_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	reg := newNodeRegistry(map[string]node.Conn{"seed": &fakeCapNode{id: "seed"}})
	reg.SetKnown("seed", "Seed")

	const workers = 8
	const iters = 200
	var wg sync.WaitGroup
	wg.Add(workers * 2)
	for w := 0; w < workers; w++ {
		id := fmt.Sprintf("w%d", w)
		go func() { // writer
			defer wg.Done()
			for i := 0; i < iters; i++ {
				reg.Add(id, &fakeCapNode{id: id})
				reg.Remove(id)
			}
		}()
		go func() { // reader
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_ = reg.HasNodes()
				_ = reg.Len()
				_, _ = reg.NodeByID(id)
				_ = reg.NodesSnapshot()
				_ = reg.Conns()
				_ = reg.NodesStatus()
				_ = reg.appendConns(func(n int) []node.Conn { return make([]node.Conn, 0, n) })
				_ = reg.KnownNodes()
			}
		}()
	}
	wg.Wait()

	if reg.Len() != 1 {
		t.Errorf("after churn Len = %d, want 1 (only the seed survives)", reg.Len())
	}
	if _, ok := reg.NodeByID("seed"); !ok {
		t.Error("seed node lost during concurrent churn")
	}
}
