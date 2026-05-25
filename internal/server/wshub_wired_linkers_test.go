package server

import (
	"os"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session/agentlink"
)

// stubAgentLinker is the minimal AgentLinker used by the dedup contract
// test below. Method bodies are noops because the test only inspects
// map-key identity, not callback dispatch.
type stubAgentLinker struct {
	id string
}

func (s *stubAgentLinker) OnResolve(fn func(taskID, toolUseID, internalAgentID string)) {
}
func (s *stubAgentLinker) Query(taskID string) (cli.LinkInfo, bool) { return cli.LinkInfo{}, false }
func (s *stubAgentLinker) QueryOrResolveFast(taskID string) (cli.LinkInfo, bool) {
	return cli.LinkInfo{}, false
}
func (s *stubAgentLinker) ProjectSessionDir() string { return "" }

// secondStubAgentLinker is structurally identical to stubAgentLinker but
// has a different dynamic type. Used to assert that the wiredLinkers map
// keys two same-pointer-value-but-different-type linkers as separate
// entries — the documented contract under R248-GO-5 (issue #372).
type secondStubAgentLinker struct {
	id string
}

func (s *secondStubAgentLinker) OnResolve(fn func(taskID, toolUseID, internalAgentID string)) {
}
func (s *secondStubAgentLinker) Query(taskID string) (cli.LinkInfo, bool) {
	return cli.LinkInfo{}, false
}
func (s *secondStubAgentLinker) QueryOrResolveFast(taskID string) (cli.LinkInfo, bool) {
	return cli.LinkInfo{}, false
}
func (s *secondStubAgentLinker) ProjectSessionDir() string { return "" }

// TestWiredLinkers_DedupContract pins the dedup semantics of
// wiredLinkers map[agentlink.AgentLinker]struct{} for issue #372:
//
//  1. Same pointer + same dynamic type → 1 entry (idempotent re-wire).
//  2. Different pointer + same dynamic type → 2 entries (each linker
//     registers its own callbacks).
//  3. Same pointer value + different dynamic type → 2 entries (the
//     interface key tuple is (T, V), not just V — this is *correct*
//     because two different concrete types are observably different
//     objects, but it is the foot-gun documented in the wiredLinkers
//     godoc that future multi-backend authors must understand: an
//     adapter that wraps a canonical AgentLinker without changing
//     identity will produce a duplicate slot for the same underlying
//     linker. Producers MUST satisfy 1:1 (one canonical AgentLinker
//     per owner) or this dedup silently double-fires OnResolve.
func TestWiredLinkers_DedupContract(t *testing.T) {
	t.Parallel()

	// Use the raw map type so the test exercises the same key-identity
	// contract that wshub.go relies on. Hub initialisation isn't needed —
	// we are pinning Go map semantics for the declared type.
	m := map[agentlink.AgentLinker]struct{}{}

	a := &stubAgentLinker{id: "a"}

	// Property 1: same pointer + same type → 1 entry.
	m[a] = struct{}{}
	m[a] = struct{}{}
	if len(m) != 1 {
		t.Errorf("same pointer+type must dedup to 1 entry, got len=%d", len(m))
	}

	// Property 2: different pointer + same type → 2 entries.
	b := &stubAgentLinker{id: "b"}
	m[b] = struct{}{}
	if len(m) != 2 {
		t.Errorf("distinct pointers must produce 2 entries, got len=%d", len(m))
	}

	// Property 3: different dynamic type → separate entries even when
	// the producer might believe the underlying object is the same.
	// We can't actually construct two interface values with the same
	// unsafe.Pointer but different dynamic types without unsafe — instead
	// we use two structurally-identical types to demonstrate that the
	// dynamic-type half of the key tuple participates in dedup. This is
	// the contract the godoc warns multi-backend authors about.
	var c agentlink.AgentLinker = &secondStubAgentLinker{id: "a"}
	m[c] = struct{}{}
	if len(m) != 3 {
		t.Errorf("distinct dynamic types must produce a new entry, got len=%d (the (T,V) key tuple regressed to V-only — wshub dedup is no longer type-aware)", len(m))
	}

	// Sanity: lookups must respect both halves of the key tuple.
	if _, ok := m[a]; !ok {
		t.Error("first stubAgentLinker entry missing after multi-type insert")
	}
	if _, ok := m[c]; !ok {
		t.Error("secondStubAgentLinker entry missing after multi-type insert")
	}
}

// TestWiredLinkers_GodocWarnsMultiBackend pins the comment that warns
// future multi-backend authors about the (T, V) dedup tuple. If the
// warning prose disappears (likely cause: a refactor that "tidies up"
// the comment), the test fails so the next maintainer is forced to
// re-read the contract before deciding whether to keep, weaken, or
// strengthen the protection. The contract belongs in the source — not
// a wiki page that drifts — so we check via os.ReadFile.
func TestWiredLinkers_GodocWarnsMultiBackend(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("wshub.go")
	if err != nil {
		t.Fatalf("read wshub.go: %v", err)
	}
	src := string(data)
	// Anchored on the unique R248 token + the multi-backend warning so a
	// random refactor of unrelated comments doesn't false-positive here.
	if !strings.Contains(src, "R248-GO-5") {
		t.Error("wshub.go: R248-GO-5 anchor missing from wiredLinkers godoc — issue #372 warning lost")
	}
	if !strings.Contains(src, "1:1 invariant") {
		t.Error("wshub.go: multi-backend 1:1 invariant warning lost from wiredLinkers godoc")
	}
	if !strings.Contains(src, "canonical AgentLinker") {
		t.Error("wshub.go: 'canonical AgentLinker' guidance lost from wiredLinkers godoc")
	}
}
