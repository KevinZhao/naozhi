package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/node"
)

// fakeCapNode is a node.Conn stub whose only relevant surface is
// Meta() — selectNodeForBackend never touches the RPC methods, so we
// reduce the interface impl to no-ops everywhere else. Mirrors the
// pattern in cache_test.go's stubConn but lives in package server so
// we can build it inline without exporting capsFromSlice equivalents.
type fakeCapNode struct {
	id   string
	caps map[string]bool
}

func (f *fakeCapNode) NodeID() string      { return f.id }
func (f *fakeCapNode) DisplayName() string { return f.id }
func (f *fakeCapNode) RemoteAddr() string  { return "fake" }
func (f *fakeCapNode) Status() string      { return "ok" }
func (f *fakeCapNode) Meta() *node.NodeMeta {
	return &node.NodeMeta{NodeID: f.id, Capabilities: f.caps}
}
func (f *fakeCapNode) Close() {}

func (f *fakeCapNode) FetchSessions(_ context.Context) ([]map[string]any, error) {
	return nil, nil
}
func (f *fakeCapNode) FetchProjects(_ context.Context) ([]map[string]any, error) {
	return nil, nil
}
func (f *fakeCapNode) FetchDiscovered(_ context.Context) ([]map[string]any, error) {
	return nil, nil
}
func (f *fakeCapNode) FetchDiscoveredPreview(_ context.Context, _ string) ([]cli.EventEntry, error) {
	return nil, nil
}
func (f *fakeCapNode) FetchEvents(_ context.Context, _ string, _ int64) ([]cli.EventEntry, error) {
	return nil, nil
}
func (f *fakeCapNode) Send(_ context.Context, _, _, _ string) error { return nil }
func (f *fakeCapNode) ProxyTakeover(_ context.Context, _ int, _, _ string, _ uint64) (string, error) {
	return "", nil
}
func (f *fakeCapNode) ProxyCloseDiscovered(_ context.Context, _ int, _, _ string, _ uint64) error {
	return nil
}
func (f *fakeCapNode) ProxyRestartPlanner(_ context.Context, _ string) error { return nil }
func (f *fakeCapNode) ProxyUpdateConfig(_ context.Context, _ string, _ json.RawMessage) error {
	return nil
}
func (f *fakeCapNode) ProxySetFavorite(_ context.Context, _ string, _ bool) error { return nil }
func (f *fakeCapNode) ProxyRemoveSession(_ context.Context, _ string) (bool, error) {
	return true, nil
}
func (f *fakeCapNode) ProxyInterruptSession(_ context.Context, _ string) (bool, error) {
	return true, nil
}
func (f *fakeCapNode) ProxySetSessionLabel(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}
func (f *fakeCapNode) Subscribe(_ node.EventSink, _ string, _ int64) {}
func (f *fakeCapNode) Unsubscribe(_ node.EventSink, _ string)        {}
func (f *fakeCapNode) RefreshSubscription(_ string)                  {}
func (f *fakeCapNode) RemoveClient(_ node.EventSink)                 {}

// mapLookup is the tiniest nodeLookup that selectNodeForBackend
// accepts: a static id → Conn map. Avoids constructing a full Server
// or Hub for what is fundamentally a pure dispatch decision.
type mapLookup map[string]node.Conn

func (m mapLookup) NodeByID(id string) (node.Conn, bool) {
	nc, ok := m[id]
	return nc, ok
}

// withDefaultBackends ensures backend.RegisterDefaults has been called
// exactly once for the test process. Matches the pattern other tests
// in this package use (replyTagForBackendOnce in server.go) — the
// registry is process-global, so registering twice panics. sync.Once
// is the safe seed.
var withDefaultBackendsOnce sync.Once

func withDefaultBackends(t *testing.T) {
	t.Helper()
	withDefaultBackendsOnce.Do(func() {
		// Tolerate prior registration by other tests in the same
		// process (e.g. server_test, replyTagForBackend lazy init):
		// only register if the registry is empty.
		if len(backend.All()) == 0 {
			backend.RegisterDefaults()
		}
	})
}

// TestSelectNodeForBackend_TableDriven covers the four scenarios the
// Sprint 6b ticket lists, plus the empty-target / unknown-backend
// edge cases that the helper handles for legacy single-backend
// deployments and operator typo respectively.
//
// The matrix uses fakeCapNode so we exercise the cap lookup path
// without touching any websocket or rpc machinery. backend.Profile
// for "claude" (RequiredNodeCaps==nil) and "kiro" (["acp"]) are
// resolved from the live registry seeded by withDefaultBackends.
func TestSelectNodeForBackend_TableDriven(t *testing.T) {
	withDefaultBackends(t)

	acpNode := &fakeCapNode{id: "acp-node", caps: map[string]bool{"acp": true}}
	plainNode := &fakeCapNode{id: "plain-node", caps: nil}
	lookup := mapLookup{
		"acp-node":   acpNode,
		"plain-node": plainNode,
	}

	cases := []struct {
		name      string
		target    string
		backendID string
		wantNC    node.Conn
		wantErrIs error
		wantErrIn string
	}{
		{
			name:      "claude_anyNode_passes",
			target:    "plain-node",
			backendID: "claude",
			wantNC:    plainNode,
		},
		{
			name:      "claude_acpNodeAlsoPasses",
			target:    "acp-node",
			backendID: "claude",
			wantNC:    acpNode,
		},
		{
			name:      "kiro_acpNode_passes",
			target:    "acp-node",
			backendID: "kiro",
			wantNC:    acpNode,
		},
		{
			name:      "kiro_plainNode_rejected",
			target:    "plain-node",
			backendID: "kiro",
			wantErrIs: ErrNodeMissingCap,
			wantErrIn: "acp",
		},
		{
			name:      "kiro_unknownNode_rejected",
			target:    "missing-node",
			backendID: "kiro",
			wantErrIs: ErrNodeNotConnected,
		},
		{
			name:      "emptyBackend_legacyPath",
			target:    "plain-node",
			backendID: "",
			wantNC:    plainNode,
		},
		{
			name:      "unknownBackend_rejected",
			target:    "acp-node",
			backendID: "futurebackend",
			wantErrIs: ErrUnknownBackend,
		},
		{
			name:   "emptyTarget_localDispatch",
			target: "",
		},
		{
			name:   "localTarget_localDispatch",
			target: "local",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			nc, err := selectNodeForBackend(lookup, tc.target, tc.backendID)
			if tc.wantErrIs != nil {
				if err == nil {
					t.Fatalf("expected error %v, got nil (nc=%v)", tc.wantErrIs, nc)
				}
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("err = %v; want errors.Is %v", err, tc.wantErrIs)
				}
				if tc.wantErrIn != "" && !strings.Contains(err.Error(), tc.wantErrIn) {
					t.Errorf("err = %q; want contains %q", err.Error(), tc.wantErrIn)
				}
				if nc != nil {
					t.Errorf("nc = %v; want nil on error path", nc)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if nc != tc.wantNC {
				t.Errorf("nc = %v; want %v", nc, tc.wantNC)
			}
		})
	}
}

// TestSelectNodeForBackend_AllRequiredCapsChecked guarantees the
// helper does not stop at the first cap when a future backend grows
// multiple RequiredNodeCaps. Synthetic profile registration so we
// don't depend on RegisterDefaults shipping a multi-cap backend.
func TestSelectNodeForBackend_AllRequiredCapsChecked(t *testing.T) {
	// Snapshot + restore is awkward against the unexported reset(),
	// so we register a fresh profile under a unique ID. Register
	// panics on duplicate id (intentional); use sync.Once to gate.
	multiCapOnce.Do(func() {
		backend.Register(backend.Profile{
			ID:               "synthetic-multicap",
			DisplayName:      "synth",
			DefaultBinary:    "synth",
			DefaultTag:       "syn",
			NewProtocol:      func(_ backend.ProtocolDeps) cli.Protocol { return nil },
			DetectInProc:     func(_ string) bool { return false },
			RequiredNodeCaps: []string{"capA", "capB"},
		})
	})

	partial := &fakeCapNode{id: "partial", caps: map[string]bool{"capA": true}}
	full := &fakeCapNode{id: "full", caps: map[string]bool{"capA": true, "capB": true}}
	lookup := mapLookup{"partial": partial, "full": full}

	if _, err := selectNodeForBackend(lookup, "full", "synthetic-multicap"); err != nil {
		t.Fatalf("full-cap node should pass: %v", err)
	}

	_, err := selectNodeForBackend(lookup, "partial", "synthetic-multicap")
	if !errors.Is(err, ErrNodeMissingCap) {
		t.Fatalf("partial-cap node should fail with ErrNodeMissingCap, got %v", err)
	}
	// The error string must name the missing cap so dashboards can
	// render an actionable message.
	if !strings.Contains(err.Error(), "capB") {
		t.Errorf("err should name missing capB; got %q", err.Error())
	}
}

var multiCapOnce sync.Once

// TestHubNodeLookup_Adapter asserts the Hub-bound lookup adapter
// finds connected nodes via the existing Hub.nodes map under
// nodesMu. Trivial wiring, but missing it would silently break the
// WS handleRemoteSend path.
func TestHubNodeLookup_Adapter(t *testing.T) {
	withDefaultBackends(t)
	acp := &fakeCapNode{id: "acp", caps: map[string]bool{"acp": true}}
	var nodesMu sync.RWMutex
	h := &Hub{
		nodes:   map[string]node.Conn{"acp": acp},
		nodesMu: &nodesMu,
	}
	lookup := hubNodeLookup{h: h}
	got, ok := lookup.NodeByID("acp")
	if !ok || got != acp {
		t.Fatalf("hubNodeLookup miss: ok=%v conn=%v", ok, got)
	}
	if _, ok := lookup.NodeByID("missing"); ok {
		t.Errorf("hubNodeLookup hit on missing id")
	}
}
