package node

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/wsproto"
)

// EventSink can receive JSON event messages pushed from a remote session.
// Implemented by server-side wsClient to receive events from nodes.
type EventSink interface {
	SendJSON(v any)
	SendRaw(data []byte)
}

// Conn is composed of four role interfaces so consumers (and test fakes)
// depend only on the slice they use (#435).

// NodeInfo exposes the register-time identity / status of a remote node.
type NodeInfo interface {
	NodeID() string
	DisplayName() string
	RemoteAddr() string
	Status() string // "ok" | "error" | "connecting"
	// Meta returns the register-time NodeMeta used to gate backend routing on
	// advertised capabilities; never nil, HasCap is the canonical lookup.
	Meta() *NodeMeta
}

// NodeFetcher pulls read-only snapshots (sessions / projects / discovered /
// events / backends) from a remote node, plus Send.
type NodeFetcher interface {
	FetchSessions(ctx context.Context) ([]map[string]any, error)
	FetchProjects(ctx context.Context) ([]map[string]any, error)
	FetchDiscovered(ctx context.Context) ([]map[string]any, error)
	FetchDiscoveredPreview(ctx context.Context, sessionID string) ([]clievent.EventEntry, error)
	FetchEvents(ctx context.Context, key string, after int64) ([]clievent.EventEntry, error)
	// FetchEventsPage reads one bounded page; a peer predating it answers
	// with its whole log and a nil HasMore.
	FetchEventsPage(ctx context.Context, key string, q EventsQuery) (EventsPage, error)
	// FetchBackends returns the remote /api/cli/backends payload verbatim as
	// raw JSON so the primary need not track a newer peer's manifest shape;
	// peers predating the RPC error and the picker collapses to single-backend.
	FetchBackends(ctx context.Context) (json.RawMessage, error)
	// Send returns how the node took the message (see SendResult). A pick the
	// link cannot carry is an error, never dropped; ErrSendUnknown means the
	// request went out with a TurnID and no answer came back.
	Send(ctx context.Context, req SendRequest) (SendResult, error)
}

// SendRequest is one message the primary forwards to a node's session.
// Backend and ResumeID are the spawn-time picks the node records for Key, as
// a local send does. TurnID is the browser's send ID: a node that knows it
// answers a repeat with the first answer instead of running a second turn.
type SendRequest struct {
	Key       string
	Text      string
	Workspace string
	Backend   string
	ResumeID  string
	TurnID    string
}

// SendResult is the node's answer to a send. Status is "accepted", "queued",
// "reset" or "busy" (see sendStatus); Dup marks a TurnID the node had already
// taken, Status then being the first send's.
type SendResult struct {
	Status string
	Dup    bool
}

// ErrSendUnknown is a send that went out with a TurnID and got no answer
// (timeout or a dropped link): the node may be running it. Retrying with the
// same TurnID is safe.
var ErrSendUnknown = errors.New("远程节点未及时回应，消息状态未知")

// ErrSendBackendUnsupported refuses a send whose backend or resume pick the
// node cannot read; dropping it would start the session on the wrong backend.
var ErrSendBackendUnsupported = errors.New("远程节点版本过旧，不支持指定后端或续接会话，请升级该节点")

// MaxTurnIDLen bounds a turn ID; the browser's are 33 bytes.
const MaxTurnIDLen = 64

// ValidTurnID reports whether id is a usable turn ID: 1..MaxTurnIDLen bytes
// of [A-Za-z0-9._-], so it is safe in logs, headers and dedup keys.
func ValidTurnID(id string) bool {
	if id == "" || len(id) > MaxTurnIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// sendStatus is the admission status a node answered a send with, limited to
// the values the dashboard's send_ack understands. A peer that answers
// nothing or something else took the message the only way older nodes
// could, so it reads as "accepted".
func sendStatus(answer string) string {
	switch answer {
	case "queued", "reset", "busy":
		return answer
	default:
		return "accepted"
	}
}

// sendResultFrom decodes a node's {"status": …, "dup": …} send answer; a
// body that is absent or does not decode reads as "accepted".
func sendResultFrom(body []byte) SendResult {
	var v struct {
		Status string `json:"status"`
		Dup    bool   `json:"dup"`
	}
	_ = json.Unmarshal(body, &v)
	return SendResult{Status: sendStatus(v.Status), Dup: v.Dup}
}

// EventsQuery selects a history page: Before > 0 asks for the newest Limit
// entries strictly older than Before, otherwise the opening page sized by
// Limit (a page-size hint, as on subscribe).
type EventsQuery struct {
	Before int64
	Limit  int
}

// EventsPage is a history page and whether older history exists; HasMore is
// nil when the node cannot tell.
type EventsPage struct {
	Events  []clievent.EventEntry
	HasMore *bool
}

// errHistoryUnavailable is the keyed error a sink gets when its history
// fetch fails, so the dashboard offers a retry instead of a blank pane.
const errHistoryUnavailable = "history unavailable"

func historyUnavailable(nodeID, key string) wsproto.Error {
	return wsproto.NewError(wsproto.Error{Key: key, Node: nodeID, Error: errHistoryUnavailable})
}

// fetchHistory reads a sink's own history: the catch-up from after, else the
// opening page sized by limit (<= 0 means the largest page).
func fetchHistory(ctx context.Context, f NodeFetcher, key string, after int64, limit int) (EventsPage, error) {
	if after > 0 {
		entries, err := f.FetchEvents(ctx, key, after)
		return EventsPage{Events: entries}, err
	}
	if limit <= 0 || limit > maxPushedHistoryEvents {
		limit = maxPushedHistoryEvents
	}
	return f.FetchEventsPage(ctx, key, EventsQuery{Limit: limit})
}

// sendHistoryPage hands cl its own page as an Initial frame. A page that
// knows HasMore goes out even when empty, as the local hub's does; an empty
// legacy answer sends nothing.
func sendHistoryPage(cl EventSink, nodeID, key string, p EventsPage) {
	if len(p.Events) > 0 || p.HasMore != nil {
		cl.SendJSON(wsproto.NewHistory(wsproto.History{Key: key, Node: nodeID, Events: p.Events, Initial: true, HasMore: p.HasMore}))
	}
}

// NodeProxy forwards state-mutating dashboard RPCs (takeover / close /
// config / favorite / remove / interrupt / label) to a remote node.
type NodeProxy interface {
	ProxyTakeover(ctx context.Context, pid int, sessionID, cwd string, procStart uint64) (string, error)
	ProxyCloseDiscovered(ctx context.Context, pid int, sessionID, cwd string, procStart uint64) error
	ProxyUpdateConfig(ctx context.Context, projectName string, cfg json.RawMessage) error
	ProxySetFavorite(ctx context.Context, projectName string, favorite bool) error
	// ProxyRemoveSession: (true, nil) removed; (false, nil) remote 404;
	// (false, err) transport error.
	ProxyRemoveSession(ctx context.Context, key string) (bool, error)
	// ProxyInterruptSession: (true, nil) interrupted; (false, nil) not running;
	// (false, err) transport error.
	ProxyInterruptSession(ctx context.Context, key string) (bool, error)
	// ProxySetSessionLabel: (true, nil) updated; (false, nil) remote 404;
	// (false, err) transport error or peer predating the RPC.
	ProxySetSessionLabel(ctx context.Context, key, label string) (bool, error)
}

// NodeSubscriber manages per-client event subscriptions against a remote node.
// Subscribe's after and limit are the browser's subscribe fields: catch-up
// watermark, and page-size hint for the opening page when after is 0.
type NodeSubscriber interface {
	Subscribe(c EventSink, key string, after int64, limit int)
	Unsubscribe(c EventSink, key string)
	RefreshSubscription(key string)
	RemoveClient(c EventSink)
}

// Conn is the unified interface for direct (HTTPClient) and reverse-connected
// (ReverseConn) remote nodes: the four role interfaces plus Close.
type Conn interface {
	NodeInfo
	NodeFetcher
	NodeProxy
	NodeSubscriber

	Close()
}

// containsSink reports whether c is already in clients, keeping same-sink
// re-subscribes idempotent (otherwise every fan-out reaches it twice). Caller
// holds the lock protecting the slice.
func containsSink(clients []EventSink, c EventSink) bool {
	for _, cl := range clients {
		if cl == c {
			return true
		}
	}
	return false
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
