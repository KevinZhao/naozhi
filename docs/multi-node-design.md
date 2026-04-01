# Multi-Node Design

One primary naozhi aggregates sessions from remote naozhi instances over HTTP/WS.

## Concept

```
Remote Node A (macbook)          Primary Node (EC2)
naozhi :8180                     naozhi :8180
claude CLI                       claude CLI
  ^                                ^     ^
  |  HTTP/WS over any network      |     |
  +--------------------------------+   Browser / Feishu
```

Prerequisite: primary can reach remote nodes via HTTP (WG, Tailscale, LAN, SSH tunnel, public IP — user's choice).

## Config

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

## API Changes

All changes are additive. Existing API works unchanged when `nodes` is not configured.

### GET /api/sessions

Primary fetches from all remote nodes in parallel, merges with local sessions.

Response adds `node` field to each snapshot:

```json
{
  "sessions": [
    {"key": "feishu:direct:alice:general", "state": "running", "node": "local", ...},
    {"key": "feishu:direct:bob:general", "state": "ready", "node": "macbook", ...}
  ],
  "nodes": {
    "local": {"display_name": "EC2 Tokyo", "status": "ok"},
    "macbook": {"display_name": "MacBook Pro", "status": "ok"}
  }
}
```

If a remote node is unreachable, its status is `"error"` and its sessions are omitted (no blocking).

### GET /api/sessions/events?key=...&node=macbook

If `node` is empty or `local`, use local router (existing behavior).
If `node` is a remote name, proxy to `GET {node.url}/api/sessions/events?key=...`.

### POST /api/sessions/send

Body adds optional `node` field:

```json
{"key": "feishu:direct:bob:general", "text": "hello", "node": "macbook"}
```

If `node` is remote, proxy to `POST {node.url}/api/sessions/send` with bearer token auth.

### WS subscribe

```json
{"type": "subscribe", "key": "...", "node": "macbook"}
```

Hub maintains one relay WS connection per remote node (lazy, on first subscribe). Events from remote node are relayed to all local browser clients subscribed to that session.

### WS send

```json
{"type": "send", "key": "...", "text": "hello", "node": "macbook", "id": "r1"}
```

Proxied via HTTP POST to remote node (simpler than WS-to-WS forwarding for sends).

## Architecture

### NodeClient

HTTP + WS client for one remote node.

```go
type NodeClient struct {
    ID          string
    URL         string
    Token       string
    DisplayName string
    httpClient  *http.Client
}

func (n *NodeClient) FetchSessions(ctx context.Context) ([]session.SessionSnapshot, error)
func (n *NodeClient) FetchEvents(ctx context.Context, key string, after int64) ([]cli.EventEntry, error)
func (n *NodeClient) Send(ctx context.Context, key, text string) error
func (n *NodeClient) Health(ctx context.Context) error
```

### WS Relay

Per-node relay that maintains a persistent WS connection to the remote node.

```go
type wsRelay struct {
    node       *NodeClient
    mu         sync.Mutex
    conn       *websocket.Conn
    subs       map[string][]*wsClient  // remote key -> local clients
    done       chan struct{}
}

func (r *wsRelay) Subscribe(client *wsClient, key string, after int64)
func (r *wsRelay) Unsubscribe(client *wsClient, key string)
func (r *wsRelay) Close()
```

Lifecycle:
1. First subscribe to a remote session creates the relay and opens WS
2. Relay authenticates with remote node's token
3. Relay subscribes on behalf of local clients
4. Events from remote are fanned out to local clients
5. Last unsubscribe from a key sends unsubscribe to remote
6. Relay reconnects on disconnect (exponential backoff)

### Hub Changes

```go
type Hub struct {
    ...existing fields...
    nodes   map[string]*NodeClient  // node ID -> client
    relays  map[string]*wsRelay     // node ID -> WS relay (lazy)
}
```

`handleSubscribe`: if `msg.Node != "" && msg.Node != "local"`, delegate to relay.
`handleSend`: if `msg.Node` is remote, proxy via NodeClient.Send().

### Dashboard Changes

- `handleAPISessions`: parallel fetch from all nodes (5s timeout per node), merge.
- `handleAPISessionEvents`: route by `node` param.
- `handleAPISend`: route by `node` field.

### Frontend Changes

- Session cards show `[macbook]` node badge.
- Node status in sidebar header (green/red per node).
- Subscribe/send include `node` field.
- Node filter dropdown (optional, v2).

## File Change Map

| File | Change | ~Lines |
|------|--------|--------|
| `internal/config/config.go` | Add `NodeConfig` struct, `Nodes` field | +30 |
| `internal/server/nodeclient.go` | **New**: HTTP client for remote nodes | +150 |
| `internal/server/nodeclient_test.go` | **New**: Tests with httptest mock | +200 |
| `internal/server/wsrelay.go` | **New**: WS relay for remote event streaming | +200 |
| `internal/server/wsrelay_test.go` | **New**: Relay tests | +150 |
| `internal/server/dashboard.go` | Aggregation + routing by node | +80 |
| `internal/server/wshub.go` | Add nodes/relays, route subscribe/send | +40 |
| `internal/server/wshandler.go` | Add `Node` field to wsClientMsg/wsServerMsg | +4 |
| `internal/server/server.go` | Pass nodes config to Hub | +10 |
| `internal/session/managed.go` | Add `Node` to SessionSnapshot | +2 |
| `dashboard.html` | Node badges, node in subscribe/send | +60 |
| **Total** | | **~930** |

## Implementation Phases

### Phase 1: Config + NodeClient + API Aggregation
- Parse `nodes` from config
- NodeClient HTTP methods (FetchSessions, FetchEvents, Send, Health)
- handleAPISessions: parallel fetch + merge
- handleAPISessionEvents: route by node
- handleAPISend: route by node
- Tests: mock remote node with httptest

### Phase 2: WS Relay
- wsRelay struct with persistent connection
- Hub integration: route subscribe/send by node
- Relay reconnection on disconnect
- Tests: relay subscribe, event forwarding, disconnect/reconnect

### Phase 3: Frontend
- Session cards with node badge
- Node status display
- Subscribe/send with node field
- WS message routing for remote sessions

## Phase 3: Frontend — Detail

### What's Done

`dashboard.html` already implements the core multi-node UI:

| Feature | Where |
|---------|-------|
| `isMultiNode()` helper | JS line ~1191 |
| `nodesData` populated from `GET /api/sessions` `.nodes` field | `fetchSessions()` |
| `renderNodeGroups()` — node → session with status dot | sidebar |
| `renderThreeLevel()` — node → project → session (when both flags true) | sidebar |
| `selectedNode` tracked alongside `selectedKey` throughout | `selectSession()` |
| WS subscribe/unsubscribe include `node` field | `wsm.subscribe()` |
| WS send includes `node` field | `wsm.send()` |
| HTTP events `?node=` routing | `fetchEvents()` |
| HTTP send `body.node` routing | `sendMessage()` |
| Node info in main detail header | `renderMainShell()` |
| `.node-badge`, `.node-dot`, `.node-group` CSS | stylesheet |

### Known Bugs ~~(must fix before shipping)~~ — All Fixed

**Bug 1 — `renderSelectedOrphanCard` undefined** — **FIXED** (2026-03-31)

函数已定义在 dashboard.html:598。

**Bug 2 — local node dot always red in `renderNodeGroups()`** — **FIXED** (2026-03-31)

dashboard.html:569 和 620 均已添加 `|| nid === 'local'` 检查。

### Remaining Work

| Item | Status |
|------|--------|
| ~~Fix `renderSelectedOrphanCard` (Bug 1)~~ | Done |
| ~~Fix local node red dot in `renderNodeGroups` (Bug 2)~~ | Done |
| Node badge on individual session cards (`.node-badge` CSS unused) | Nice-to-have |
| Node filter dropdown | v2, optional |

The node badge is only strictly needed when sessions from different nodes appear in the same
flat list (e.g. unmatched group in two-level view). In three-level view the group header
provides context. Low priority.

## Verification

### Existing Test Coverage

**Phase 1 (NodeClient + API aggregation):**
`nodeclient_test.go` — FetchSessions, FetchEvents, Send, Health, FetchProjects, FetchDiscovered,
ProxyTakeover, ProxyRestartPlanner, ProxyUpdateConfig (httptest mock).

**Phase 2 (WS Relay + Hub):**
`wsrelay_test.go` — ConnectAndSubscribe, EventForwarding, MultipleClients, Unsubscribe, Close,
Reconnect (3s), AuthFailed, RemoveClient, Hub_RemoteSubscribe, Hub_RemoteSubscribe_UnknownNode,
Hub_RemoteUnsubscribe_NoRelay.

### Missing Unit Tests

Add to `dashboard_test.go`:

```go
// TestHandleAPISessions_NodeAggregation: mock remote node serving /api/sessions,
// call refreshNodeCache(), then GET /api/sessions — verify merged sessions carry node="remote".
func TestHandleAPISessions_NodeAggregation(t *testing.T) { ... }

// TestHandleAPISessionEvents_RemoteNode: GET /api/sessions/events?key=x&node=remote
// should proxy to NodeClient.FetchEvents, not local router.
func TestHandleAPISessionEvents_RemoteNode(t *testing.T) { ... }

// TestHandleAPISend_RemoteNode: POST /api/sessions/send {"key":"x","node":"remote","text":"hi"}
// should call NodeClient.Send, not local router.
func TestHandleAPISend_RemoteNode(t *testing.T) { ... }
```

### Integration Test — Two Processes, One Machine

No second machine needed. Run two naozhi instances on different ports.

**Config files:**

```yaml
# config-node-b.yaml  (the "remote")
server:
  addr: ":8181"
dashboard_token: "test-secret"
cli:
  path: "~/.local/bin/claude"
session:
  max_procs: 1
```

```yaml
# config-node-a.yaml  (the primary / dashboard viewer)
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

**Verification script:**

```bash
# 1. Start both nodes
./naozhi --config config-node-b.yaml &; NODE_B=$!
./naozhi --config config-node-a.yaml &; NODE_A=$!
sleep 2

# 2. Node A's /api/sessions must include nodes map with node-b
curl -s http://localhost:8180/api/sessions | jq '.nodes'
# expect: {"local":{...},"node-b":{"status":"ok","display_name":"Node B (local)"}}

# 3. Create a session on Node B directly
curl -s -X POST http://localhost:8181/api/sessions/send \
  -H "Authorization: Bearer test-secret" \
  -H "Content-Type: application/json" \
  -d '{"key":"dashboard:d:tester:general","text":"echo hello"}'

# 4. After cache refresh (~10s), Node A must aggregate it
curl -s http://localhost:8180/api/sessions | jq '.sessions[] | {key,node}'
# expect: ...{"key":"dashboard:d:tester:general","node":"node-b"}

# 5. Node A can fetch its events
curl -s "http://localhost:8180/api/sessions/events?key=dashboard:d:tester:general&node=node-b"

# 6. Node A can send to it
curl -s -X POST http://localhost:8180/api/sessions/send \
  -H "Content-Type: application/json" \
  -d '{"key":"dashboard:d:tester:general","text":"hello from A","node":"node-b"}'

# Cleanup
kill $NODE_A $NODE_B
```

**Dashboard UI checklist (manual, with both nodes running):**

- [ ] Sidebar shows two groups: "Node A" and "Node B (local)" with green dots
- [ ] Node B group is red when node-b is stopped
- [ ] Clicking a Node B session subscribes via WS relay (events appear live)
- [ ] Sending a message from Node A dashboard reaches Node B's CLI
- [ ] Reconnect: kill and restart node-b → relay reconnects, events resume
- [ ] Takeover button on Node B discovered processes calls `ProxyTakeover`

### What to Do When a Test Fails

| Symptom | Likely cause |
|---------|-------------|
| Sidebar empty in multi-node+project view | `renderSelectedOrphanCard` ReferenceError (Bug 1) |
| Local node shows red dot | `renderNodeGroups` missing `|| nid === 'local'` (Bug 2) |
| Remote sessions missing from `/api/sessions` | `refreshNodeCache` not called or 5s timeout too short |
| WS events not arriving from remote | WS relay not connecting — check token, URL scheme (ws:// vs wss://) |
| Send to remote returns 500 | `ProxyUpdateConfig` / `NodeClient.Send` HTTP error — check remote token |

## Protocol Compatibility

Remote nodes run **unmodified** naozhi. The multi-node feature is purely a primary-side concern. Any naozhi instance with a dashboard_token can be a remote node.
