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
	Send(ctx context.Context, key, text string) error

	ProxyTakeover(ctx context.Context, pid int, sessionID, cwd string, procStart uint64) error
	ProxyRestartPlanner(ctx context.Context, projectName string) error
	ProxyUpdateConfig(ctx context.Context, projectName string, cfg json.RawMessage) error

	Subscribe(c wsEventSink, key string, after int64)
	Unsubscribe(c wsEventSink, key string)
	RemoveClient(c wsEventSink)

	Close()
}
