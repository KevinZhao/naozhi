package server

import (
	"context"
	"encoding/json"

	"github.com/naozhi/naozhi/internal/cli"
)

// NodeConn is the unified interface for both direct (NodeClient, HTTP) and
// reverse-connected (ReverseNodeConn, WS) remote nodes.
type NodeConn interface {
	NodeID() string
	DisplayName() string
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

	Subscribe(c *wsClient, key string, after int64)
	Unsubscribe(c *wsClient, key string)
	RemoveClient(c *wsClient)

	Close()
}
