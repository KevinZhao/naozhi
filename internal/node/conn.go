package node

import (
	"context"
	"encoding/json"

	"github.com/naozhi/naozhi/internal/cli"
)

// EventSink can receive JSON event messages pushed from a remote session.
// Implemented by server-side wsClient to receive events from nodes.
type EventSink interface {
	SendJSON(v any)
	SendRaw(data []byte)
}

// Conn is the unified interface for both direct (HTTPClient, HTTP) and
// reverse-connected (ReverseConn, WS) remote nodes.
type Conn interface {
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

	ProxyTakeover(ctx context.Context, pid int, sessionID, cwd string, procStart uint64) (string, error)
	ProxyCloseDiscovered(ctx context.Context, pid int, sessionID, cwd string, procStart uint64) error
	ProxyRestartPlanner(ctx context.Context, projectName string) error
	ProxyUpdateConfig(ctx context.Context, projectName string, cfg json.RawMessage) error
	ProxySetFavorite(ctx context.Context, projectName string, favorite bool) error
	// ProxyRemoveSession forwards DELETE /api/sessions to the remote node.
	// Returns (true, nil) when the session was removed; (false, nil) when the
	// remote responded 404 (session not found); (false, err) on transport errors.
	ProxyRemoveSession(ctx context.Context, key string) (bool, error)
	// ProxyInterruptSession forwards POST /api/sessions/interrupt to the remote node.
	// Returns (true, nil) when interrupted; (false, nil) when the remote reports
	// the session is not running; (false, err) on transport errors.
	ProxyInterruptSession(ctx context.Context, key string) (bool, error)
	// ProxySetSessionLabel forwards PATCH /api/sessions/label to the remote node.
	// Returns (true, nil) when the label was updated; (false, nil) when the remote
	// responded 404 (session not found); (false, err) on transport errors or when
	// the peer does not implement the RPC yet (older binaries).
	ProxySetSessionLabel(ctx context.Context, key, label string) (bool, error)

	Subscribe(c EventSink, key string, after int64)
	Unsubscribe(c EventSink, key string)
	RefreshSubscription(key string)
	RemoveClient(c EventSink)

	Close()
}

// removeSub removes c from subs[key]. Returns true if the key has no subscribers left.
// Caller must hold the lock protecting subs.
func removeSub(subs map[string][]EventSink, key string, c EventSink) bool {
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
func removeSubAll(subs map[string][]EventSink, c EventSink) []string {
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
