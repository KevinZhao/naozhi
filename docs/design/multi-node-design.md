# Multi-Node Design

One primary naozhi aggregates sessions from remote naozhi instances. Two transport modes are supported:

- **Direct Mode**: Primary polls remote nodes via HTTP/WS (requires network reachability).
- **Reverse Mode**: Remote nodes dial the primary over a persistent WebSocket (works behind NAT).

Both modes coexist and implement the same `NodeConn` interface. The rest of the server is transport-agnostic.

---

## Direct Mode (HTTP Polling)

### Architecture

```
Remote Node A (macbook)          Primary Node (EC2)
naozhi :8180                     naozhi :8180
claude CLI                       claude CLI
  ^                                ^     ^
  |  HTTP/WS over any network      |     |
  +--------------------------------+   Browser / Feishu
```

Prerequisite: primary can reach remote nodes via HTTP (WG, Tailscale, LAN, SSH tunnel, public IP).

### Config

```yaml
# Primary node config.yaml
nodes:
  macbook:
    url: "http://10.0.0.2:8180"
    token: "shared-secret"        # remote node's dashboard_token
    display_name: "MacBook Pro"
  dev-server:
    url: "http://10.0.1.5:8180"
    token: "another-secret"
    display_name: "Dev Server"
```

Remote nodes run stock naozhi with `dashboard_token` set. No code changes needed on remote side.

### API Changes

All changes are additive. Existing API works unchanged when `nodes` is not configured.

**GET /api/sessions** -- Primary fetches from all remote nodes in parallel, merges with local sessions. Response adds `node` field:

```json
{
  "sessions": [
    {"key": "feishu:direct:alice:general", "state": "running", "node": "local"},
    {"key": "feishu:direct:bob:general", "state": "ready", "node": "macbook"}
  ],
  "nodes": {
    "local": {"display_name": "EC2 Tokyo", "status": "ok"},
    "macbook": {"display_name": "MacBook Pro", "status": "ok"}
  }
}
```

If a remote node is unreachable, its status is `"error"` and its sessions are omitted.

**GET /api/sessions/events?key=...&node=macbook** -- If `node` is empty or `local`, uses local router. Otherwise proxies to `GET {node.url}/api/sessions/events?key=...`.

**POST /api/sessions/send** -- Body adds optional `node` field. If remote, proxies to remote node with bearer token auth.

**WS subscribe/send** -- Include `node` field to route to remote sessions:

```json
{"type": "subscribe", "key": "...", "node": "macbook"}
{"type": "send", "key": "...", "text": "hello", "node": "macbook", "id": "r1"}
```

Hub maintains one relay WS connection per remote node (lazy, on first subscribe). Sends are proxied via HTTP POST.

### NodeClient

HTTP + WS client for one remote node:

```go
type NodeClient struct {
    ID          string
    URL         string
    Token       string
    DisplayName string
    httpClient  *http.Client
    relayMu     sync.Mutex
    relay       *wsRelay  // lazy-created on first Subscribe
}
```

### WS Relay

Per-node relay maintaining a persistent WS connection to the remote:

```go
type wsRelay struct {
    node *NodeClient
    mu   sync.Mutex
    conn *websocket.Conn
    subs map[string][]*wsClient  // remote key -> local clients
    done chan struct{}
}
```

Lifecycle: first subscribe creates relay and opens WS, authenticates, subscribes on behalf of local clients, fans out events, reconnects on disconnect with exponential backoff.

### Dashboard & Frontend

- `handleAPISessions`: parallel fetch from all nodes (5s timeout per node), merge.
- `handleAPISessionEvents` / `handleAPISend`: route by `node` param/field.
- Session cards show node badge; node status in sidebar header (green/red).
- `isMultiNode()`, `renderNodeGroups()`, `renderThreeLevel()` in dashboard.html.
- All Phase 3 frontend bugs fixed.

---

## Reverse Mode (WebSocket)

### Problem

Direct mode requires the primary to reach remote nodes via HTTP/WS. Remote nodes behind NAT (home office, laptop, company intranet) are unreachable from a public EC2.

### Solution

Invert the connection direction. Remote nodes dial the primary over a persistent WebSocket on startup. All communication flows through this single outbound connection.

```
Before (pull):
  Primary (EC2) ──HTTP GET /api/sessions──► Remote (MacBook, NAT ✗)

After (reverse):
  Remote (MacBook) ──WS /ws-node──────► Primary (EC2)
  (remote only needs outbound TCP — works from any NAT)
```

### Architecture

```
Remote (MacBook, NAT)                Primary (EC2, public)
─────────────────────                ─────────────────────
Connector                            ReverseNodeServer (/ws-node)
  │                                    │
  ├─ dial wss://ec2:8180/ws-node ──►   ├─ accept, validate token
  ├─ send register{id, token}  ──────► ├─ create ReverseNodeConn
  ◄─ registered ──────────────────────┤
  │                                    │
  │   ◄──── request{fetch_sessions} ──┤  (background cache, 10s)
  ├── response{sessions:[...]} ──────► │
  │                                    │
  │   ◄──── subscribe{key=x} ─────────┤  (browser subscribes)
  ├── event{key=x, ...} ─────────────► │  (pushed on event)
```

### Message Protocol

A single WebSocket at `GET /ws-node` carries two independent flows:

**RPC (request -> response):**

```
Primary → Remote: {"type":"request","req_id":"r1","method":"fetch_sessions"}
Remote → Primary: {"type":"response","req_id":"r1","result":[...]}
Remote → Primary: {"type":"response","req_id":"r1","error":"..."}
```

Methods: `fetch_sessions`, `fetch_projects`, `fetch_discovered`, `fetch_discovered_preview`,
`fetch_events`, `send`, `takeover`, `restart_planner`, `update_config`.

**Event push (subscribe -> stream):**

```
Primary → Remote: {"type":"subscribe","key":"feishu:d:alice:g","after":0}
Remote → Primary: {"type":"subscribed","key":"..."}
Remote → Primary: {"type":"subscribe_error","key":"...","error":"..."}
Remote → Primary: {"type":"event","key":"...","event":{...}}
Remote → Primary: {"type":"session_state","key":"...","state":"ready"}
Primary → Remote: {"type":"unsubscribe","key":"..."}
```

Subscribe failures are explicit: if the session does not exist on the remote, it responds with
`subscribe_error`. Primary handles this and notifies subscribed browser clients.

**Control:**

```
Remote → Primary: {"type":"register","node_id":"macbook","token":"...","display_name":"MacBook Pro"}
Primary → Remote: {"type":"registered"} | {"type":"register_fail","error":"..."}
Remote → Primary: {"type":"sessions_changed"}
Remote ↔ Primary: {"type":"ping"} / {"type":"pong"}
```

**Go types (`internal/reverse/proto.go`):**

```go
type ReverseMsg struct {
    Type        string          `json:"type"`
    NodeID      string          `json:"node_id,omitempty"`
    Token       string          `json:"token,omitempty"`
    DisplayName string          `json:"display_name,omitempty"`
    ReqID       string          `json:"req_id,omitempty"`
    Method      string          `json:"method,omitempty"`
    Params      json.RawMessage `json:"params,omitempty"`
    Result      json.RawMessage `json:"result,omitempty"`
    Error       string          `json:"error,omitempty"`
    Key         string          `json:"key,omitempty"`
    After       int64           `json:"after,omitempty"`
    Event       *cli.EventEntry `json:"event,omitempty"`
    State       string          `json:"state,omitempty"`
}
```

---

## NodeConn Interface

The central abstraction that unifies both transport modes. `NodeClient` (direct HTTP) and
`ReverseNodeConn` (reverse WS) both implement it.

```go
// internal/server/nodeconn.go

type NodeConn interface {
    NodeID() string
    DisplayName() string
    RemoteAddr() string
    Status() string
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
```

---

### ReverseNodeConn (Primary Side)

Represents one registered reverse connection. Created by ReverseNodeServer when a remote connects.

```go
type ReverseNodeConn struct {
    id          string
    displayName string
    writeMu     sync.Mutex
    conn        *websocket.Conn
    pendingMu   sync.Mutex
    pending     map[string]chan reverseResult  // req_id -> waiting caller
    subMu       sync.Mutex
    subs        map[string][]*wsClient        // key -> local browser clients
    statusMu    sync.RWMutex
    status      string  // "ok" | "connecting" | "error"
    done        chan struct{}
}
```

RPC calls use a pending map with correlation IDs. `readLoop` dispatches responses to waiting
callers and fans out events/session_state to subscribed browser clients. Context cancellation
cleans up pending entries to prevent goroutine leaks.

Subscribe uses a lock to prevent check-then-act races: two concurrent subscribes for the same
new key send only one subscribe message to the remote.

### ReverseNodeServer (Primary Side)

Accepts `/ws-node` connections, validates token with constant-time comparison, creates
`ReverseNodeConn`.

Security: returns generic `"auth failed"` regardless of whether the node_id exists (prevents
enumeration). Per-IP rate limiting on the upgrade endpoint.

On register: replaces stale connections, triggers immediate cache refresh, notifies Hub.
On deregister: purges cached data, sets status to `"error"`, broadcasts sessions update.

### Connector (Remote Side)

`internal/connector` package. Runs when `upstream` is configured.

```go
type Connector struct {
    cfg     config.UpstreamConfig
    router  *session.Router
    projMgr *project.Manager
    scanner *discovery.Scanner
}
```

`Run(ctx)` maintains a persistent connection with exponential backoff (1s -> 30s).
`handleConn` dispatches incoming messages:

- `request`: dispatches to `handleRequest` (fetch_sessions, fetch_projects, send, takeover, etc.)
- `subscribe`: starts `streamEvents` goroutine with WaitGroup tracking
- `unsubscribe`: cancels subscription
- `ping`: responds with pong

### Reverse Mode Config

**Primary (`reverse_nodes`):**

```yaml
reverse_nodes:
  macbook:
    token: "macbook-secret-xxx"
    display_name: "MacBook Pro"
  home-desktop:
    token: "desktop-secret-xxx"
    display_name: "Home Desktop"
```

`reverse_nodes` entries have no `url` -- primary waits for them to dial in.

**Remote (`upstream`):**

```yaml
upstream:
  url: "wss://ec2.example.com:8180/ws-node"
  node_id: "macbook"                          # must match reverse_nodes key
  token: "macbook-secret-xxx"                 # must match reverse_nodes[macbook].token
  display_name: "MacBook Pro"
```

### Server / Hub Wiring

- `Server.nodes` changed from `map[string]*NodeClient` to `map[string]NodeConn`.
- `SetReverseNodeServer` registers onRegister/onDeregister callbacks.
- `refreshNodeCacheFor(id)` fetches and caches data for a single node immediately on connect.
- Hub removed the separate `relays` map; calls `NodeConn.Subscribe/Unsubscribe/RemoveClient` directly.
- On node disconnect, `PurgeNodeSubscriptions` notifies subscribed browser clients.

---

## Implementation Phases

### Direct Mode

1. **Config + NodeClient + API Aggregation**: Parse `nodes`, NodeClient HTTP methods, parallel fetch + merge in handleAPISessions, route by node in events/send endpoints.
2. **WS Relay**: wsRelay with persistent connection, Hub integration, reconnection.
3. **Frontend**: Node badges, status display, subscribe/send with node field.

### Reverse Mode

1. **NodeConn Abstraction** (no behavior change): Define interface, move wsRelay from Hub into NodeClient, change Hub/Server to use `map[string]NodeConn`.
2. **ReverseNodeConn + ReverseNodeServer** (primary side): ReverseMsg proto, RPC multiplexing, event subscription, /ws-node handler, token validation.
3. **Connector** (remote side): dial, register, handle requests, stream events, reconnect.
4. **Proactive Push** (optional): Remote sends `sessions_changed`, primary refreshes cache immediately, dashboard updates in ~100ms instead of 10s.

---

## File Change Map

| File | Change |
|------|--------|
| `internal/config/config.go` | NodeConfig, ReverseNodes, UpstreamConfig |
| `internal/reverse/proto.go` | ReverseMsg type (shared by server + connector) |
| `internal/server/nodeconn.go` | NodeConn interface |
| `internal/server/nodeclient.go` | HTTP client, Subscribe/Unsubscribe/RemoveClient/Close |
| `internal/server/nodeclient_test.go` | Tests with httptest mock |
| `internal/server/wsrelay.go` | WS relay for remote event streaming |
| `internal/server/wsrelay_test.go` | Relay tests |
| `internal/server/reverseconn.go` | ReverseNodeConn (primary-side RPC + event push) |
| `internal/server/reverseconn_test.go` | RPC + event push tests |
| `internal/server/reverseserver.go` | ReverseNodeServer, /ws-node handler, rate limit |
| `internal/connector/connector.go` | Remote-side Connector |
| `internal/connector/connector_test.go` | Connector request handling tests |
| `internal/server/dashboard.go` | Aggregation + routing by node |
| `internal/server/wshub.go` | NodeConn, remove relays, PurgeNodeSubscriptions |
| `internal/server/server.go` | NodeConn, ReverseNodeServer wiring, refreshNodeCacheFor |
| `cmd/naozhi/main.go` | Build ReverseNodeServer; start Connector |
| `dashboard.html` | Node badges, node in subscribe/send |

---

## Verification

### Unit Tests

**Direct Mode:**
- `nodeclient_test.go`: FetchSessions, FetchEvents, Send, Health, FetchProjects, FetchDiscovered, ProxyTakeover, ProxyRestartPlanner, ProxyUpdateConfig.
- `wsrelay_test.go`: ConnectAndSubscribe, EventForwarding, MultipleClients, Unsubscribe, Close, Reconnect, AuthFailed, RemoveClient, Hub_RemoteSubscribe.

**Reverse Mode:**
- `reverseconn_test.go`: RPC, ConcurrentRPC, RPCTimeout, Subscribe, EventPush, ReconnectResubs.
- `connector_test.go`: Register, RegisterBadToken, FetchSessions, Send, Subscribe, Reconnect.

### Integration Test (Two Processes, One Machine)

**Direct mode configs:**

```yaml
# config-node-b.yaml (remote)
server:
  addr: ":8181"
dashboard_token: "test-secret"

# config-node-a.yaml (primary)
server:
  addr: ":8180"
nodes:
  node-b:
    url: "http://localhost:8181"
    token: "test-secret"
    display_name: "Node B (local)"
workspace:
  id: "node-a"
  name: "Node A"
```

**Reverse mode configs:**

```yaml
# config-remote.yaml
upstream:
  url: "ws://localhost:8180/ws-node"
  node_id: "laptop"
  token: "test-secret"
  display_name: "Laptop (local test)"

# config-primary.yaml
reverse_nodes:
  laptop:
    token: "test-secret"
    display_name: "Laptop"
```

**Verification steps** (apply to both modes):

```bash
# Start both nodes
./naozhi --config config-primary.yaml &
./naozhi --config config-remote.yaml &
sleep 2

# Verify remote appears in nodes
curl -s http://localhost:8180/api/sessions | jq '.nodes'

# Verify remote sessions in aggregated list
curl -s http://localhost:8180/api/sessions | jq '.sessions[].node'

# Send from primary to remote session
curl -s -X POST http://localhost:8180/api/sessions/send \
  -H "Content-Type: application/json" \
  -d '{"key":"dashboard:d:test:general","text":"hello","node":"<node-id>"}'
```

### Dashboard UI Checklist

- Remote node appears in sidebar with green dot
- Red dot on disconnect
- Clicking remote session subscribes and streams events live
- Sending from dashboard routes to remote CLI
- Reconnect after remote/primary restart

### Troubleshooting

| Symptom | Likely cause |
|---------|-------------|
| Remote sessions missing from `/api/sessions` | Cache not refreshed or timeout too short |
| WS events not arriving from remote | Relay/reverse WS not connecting -- check token, URL scheme |
| Send to remote returns 500 | HTTP error -- check remote token |
| Local node shows red dot | Missing `nid === 'local'` check in renderNodeGroups |

---

## Compatibility

- **Direct nodes** (`workspaces`/`nodes` config) work unchanged via NodeClient.
- Both modes coexist: one node in `workspaces` (direct), another in `reverse_nodes` (reverse).
- Remote nodes run unmodified naozhi. Multi-node is purely a primary-side concern.
- Any naozhi instance with a `dashboard_token` can serve as a direct-mode remote node.
- Remote nodes on NAT use `upstream` config -- no other changes needed.
