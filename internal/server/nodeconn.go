package server

import (
	"context"
	"encoding/json"

	"github.com/naozhi/naozhi/internal/cli"
)

// wsEventSink can receive JSON event messages pushed from a remote session.
// Using an interface here avoids tying NodeConn implementations to the concrete
// *wsClient type.
type wsEventSink interface {
	sendJSON(v interface{})
	sendRaw(data []byte)
}

// NodeConn is the unified interface for both direct (NodeClient, HTTP) and
// reverse-connected (ReverseNodeConn, WS) remote nodes.
type NodeConn interface {
	NodeID() string
	DisplayName() string
	RemoteAddr() string
	Status() string // "ok" | "error" | "connecting"

	FetchSessions(ctx context.Context) ([]map[string]any, error)
	FetchProjects(ctx context.Context) ([]map[string]any, error)
	FetchDiscovered(ctx context.Context) ([]map[string]any, error)
	FetchDiscoveredPreview(ctx context.Context, sessionID string) ([]cli.EventEntry, error)
	FetchEvents(ctx context.Context, key string, after int64) ([]cli.EventEntry, error)
	Send(ctx context.Context, key, text, workspace string) error

	ProxyTakeover(ctx context.Context, pid int, sessionID, cwd string, procStart uint64) error
	ProxyRestartPlanner(ctx context.Context, projectName string) error
	ProxyUpdateConfig(ctx context.Context, projectName string, cfg json.RawMessage) error

	Subscribe(c wsEventSink, key string, after int64)
	Unsubscribe(c wsEventSink, key string)
	RemoveClient(c wsEventSink)

	Close()
}

// removeSub removes c from subs[key]. Returns true if the key has no subscribers left.
// Caller must hold the lock protecting subs.
func removeSub(subs map[string][]wsEventSink, key string, c wsEventSink) bool {
	clients := subs[key]
	for i, cl := range clients {
		if cl == c {
			subs[key] = append(clients[:i], clients[i+1:]...)
			break
		}
	}
	if len(subs[key]) == 0 {
		delete(subs, key)
		return true
	}
	return false
}

// removeSubAll removes c from all keys. Returns keys that became empty.
// Caller must hold the lock protecting subs.
func removeSubAll(subs map[string][]wsEventSink, c wsEventSink) []string {
	var emptyKeys []string
	for key, clients := range subs {
		for i, cl := range clients {
			if cl == c {
				subs[key] = append(clients[:i], clients[i+1:]...)
				break
			}
		}
		if len(subs[key]) == 0 {
			delete(subs, key)
			emptyKeys = append(emptyKeys, key)
		}
	}
	return emptyKeys
}
