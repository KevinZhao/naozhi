package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
)

// fakeCapNode is a node.Conn stub whose relevant surface is Meta() and
// FetchBackends(); every other method is a no-op. Mirrors the pattern in
// cache_test.go's stubConn but lives in package server so we can build it
// inline without exporting capsFromSlice equivalents.
type fakeCapNode struct {
	id   string
	caps map[string]bool
	// backends is the FetchBackends answer; backendsErr fails it.
	backends    json.RawMessage
	backendsErr error
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
func (f *fakeCapNode) FetchDiscoveredPreview(_ context.Context, _ string) ([]clievent.EventEntry, error) {
	return nil, nil
}
func (f *fakeCapNode) FetchEvents(_ context.Context, _ string, _ int64) ([]clievent.EventEntry, error) {
	return nil, nil
}
func (f *fakeCapNode) FetchEventsPage(_ context.Context, _ string, _ node.EventsQuery) (node.EventsPage, error) {
	return node.EventsPage{}, nil
}
func (f *fakeCapNode) FetchBackends(_ context.Context) (json.RawMessage, error) {
	return f.backends, f.backendsErr
}
func (f *fakeCapNode) Send(_ context.Context, _ node.SendRequest) (node.SendResult, error) {
	return node.SendResult{Status: "accepted"}, nil
}
func (f *fakeCapNode) ProxyTakeover(_ context.Context, _ int, _, _ string, _ uint64) (string, error) {
	return "", nil
}
func (f *fakeCapNode) ProxyCloseDiscovered(_ context.Context, _ int, _, _ string, _ uint64) error {
	return nil
}
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
func (f *fakeCapNode) Subscribe(_ node.EventSink, _ string, _ int64, _ int) {}
func (f *fakeCapNode) Unsubscribe(_ node.EventSink, _ string)               {}
func (f *fakeCapNode) RefreshSubscription(_ string)                         {}
func (f *fakeCapNode) RemoveClient(_ node.EventSink)                        {}

// mapLookup is the tiniest nodeLookup that selectNode accepts: a static
// id → Conn map.
type mapLookup map[string]node.Conn

func (m mapLookup) NodeByID(id string) (node.Conn, bool) {
	nc, ok := m[id]
	return nc, ok
}

func TestSelectNode(t *testing.T) {
	remote := &fakeCapNode{id: "remote"}
	lookup := mapLookup{"remote": remote}
	for _, tc := range []struct {
		target  string
		want    node.Conn
		wantErr error
	}{
		{"", nil, nil},
		{"local", nil, nil},
		{"remote", remote, nil},
		{"missing", nil, ErrNodeNotConnected},
	} {
		nc, err := selectNode(lookup, tc.target)
		if !errors.Is(err, tc.wantErr) || nc != tc.want {
			t.Errorf("selectNode(%q) = (%v, %v), want (%v, %v)", tc.target, nc, err, tc.want, tc.wantErr)
		}
	}
}

// TestRemoteBackend: the node's manifest decides, whatever this binary has
// registered; a pick it does not list, or a manifest it cannot serve, is
// refused rather than dropped.
func TestRemoteBackend(t *testing.T) {
	manifest := json.RawMessage(`{"backends":[{"id":"claude"},{"id":"kiro"},{"id":"future-cli"}],"default":"claude"}`)
	for _, tc := range []struct {
		name    string
		nc      *fakeCapNode
		backend string
		wantErr bool
	}{
		{"no pick needs no manifest", &fakeCapNode{backendsErr: errors.New("unknown method")}, "", false},
		{"listed", &fakeCapNode{backends: manifest}, "kiro", false},
		{"listed but unknown here", &fakeCapNode{backends: manifest}, "future-cli", false},
		{"not listed", &fakeCapNode{backends: manifest}, "codex", true},
		{"manifest unavailable", &fakeCapNode{backendsErr: errors.New("unknown method")}, "kiro", true},
		{"manifest garbage", &fakeCapNode{backends: json.RawMessage(`[1]`)}, "kiro", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := remoteBackend(context.Background(), tc.nc, tc.backend)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrNodeLacksBackend) {
				t.Errorf("err = %v, want ErrNodeLacksBackend", err)
			}
		})
	}
}

// TestHubNodeLookup_Adapter asserts the Hub-bound lookup adapter
// finds connected nodes via the shared Hub.nodes registry. Trivial
// wiring, but missing it would silently break the WS handleRemoteSend
// path.
func TestHubNodeLookup_Adapter(t *testing.T) {
	acp := &fakeCapNode{id: "acp", caps: map[string]bool{"acp": true}}
	h := &Hub{
		nodes: newNodeRegistry(map[string]node.Conn{"acp": acp}),
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
