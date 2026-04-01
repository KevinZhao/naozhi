# Reverse Connect Design

## Problem

The current multi-node architecture requires the primary node to reach remote nodes via HTTP and
WebSocket. Remote nodes behind NAT (home office, laptop, company intranet) are unreachable from a
public EC2.

## Solution

**Invert the connection direction.** Remote nodes dial the primary over a persistent WebSocket on
startup. All subsequent communication — session fetches, event subscriptions, sends — flows through
this single outbound connection. Primary never initiates connections to remotes.

```
Before (pull):
  Primary (EC2) ──HTTP GET /api/sessions──► Remote (MacBook, NAT ✗)
  Primary (EC2) ──WS /ws────────────────► Remote (MacBook, NAT ✗)

After (reverse):
  Remote (MacBook) ──WS /ws-node──────► Primary (EC2)
  (remote only needs outbound TCP — works from any NAT)
```

---

## Architecture

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
  │   ◄──── subscribe{key=x} ─────────┤  (browser subscribes to session)
  ├── event{key=x, ...} ─────────────► │  (pushed when event occurs)
  │                                    │
  ├── sessions_changed ──────────────► │  (proactive push, optional Phase 4)
```

Both direct (NodeClient, HTTP) and reverse (ReverseNodeConn, WS) modes coexist. The rest of the
server is unaware of which transport is used.

---

## Message Protocol

A single WebSocket at `GET /ws-node` carries two independent flows:

### 1. RPC (request → response)

Primary sends requests; remote handles them and responds with a correlation ID.

```
Primary → Remote: {"type":"request","req_id":"r1","method":"fetch_sessions"}
Remote → Primary: {"type":"response","req_id":"r1","result":[...]}
Remote → Primary: {"type":"response","req_id":"r1","error":"..."}  // on failure
```

Methods: `fetch_sessions`, `fetch_projects`, `fetch_discovered`,
`fetch_events` (params: `{key, after}`), `send` (params: `{key, text}`),
`takeover`, `restart_planner`, `update_config`.

### 2. Event push (subscribe → stream)

Primary subscribes to a session key; remote pushes events as they arrive.

```
Primary → Remote: {"type":"subscribe","key":"feishu:d:alice:g","after":0}
Remote → Primary: {"type":"subscribed","key":"..."}                    // session found
Remote → Primary: {"type":"subscribe_error","key":"...","error":"..."}  // session not found
Remote → Primary: {"type":"event","key":"...","event":{...}}    // repeated
Remote → Primary: {"type":"session_state","key":"...","state":"ready"}
Primary → Remote: {"type":"unsubscribe","key":"..."}
Remote → Primary: {"type":"unsubscribed","key":"..."}
```

Subscribe failures are explicit: if the session doesn't exist on the remote, it responds with
`subscribe_error` rather than silently ignoring the message. Primary must handle this and
notify subscribed browser clients.

### 3. Control

```
Remote → Primary: {"type":"register","node_id":"macbook","token":"...","display_name":"MacBook Pro"}
Primary → Remote: {"type":"registered"}           // accept
Primary → Remote: {"type":"register_fail","error":"invalid token"}  // reject
Remote → Primary: {"type":"sessions_changed"}     // proactive push (Phase 4)
Remote ↔ Primary: {"type":"ping"} / {"type":"pong"}
```

### Go types

```go
// internal/reverse/proto.go  ← shared package; both server and connector import it
// (NOT internal/server — Connector in internal/connector also needs these types)

type ReverseMsg struct {
    Type        string          `json:"type"`

    // register
    NodeID      string          `json:"node_id,omitempty"`
    Token       string          `json:"token,omitempty"`
    DisplayName string          `json:"display_name,omitempty"`

    // request / response
    ReqID  string          `json:"req_id,omitempty"`
    Method string          `json:"method,omitempty"`
    Params json.RawMessage `json:"params,omitempty"`
    Result json.RawMessage `json:"result,omitempty"`
    Error  string          `json:"error,omitempty"`

    // subscribe / event push
    Key   string         `json:"key,omitempty"`
    After int64          `json:"after,omitempty"`
    Event *cli.EventEntry `json:"event,omitempty"`
    State string          `json:"state,omitempty"`
}
```

---

## NodeConn Interface

The central abstraction. Replaces direct `*NodeClient` references throughout the server.
Both `NodeClient` (direct HTTP) and `ReverseNodeConn` (reverse WS) implement it.

```go
// internal/server/nodeconn.go

type NodeConn interface {
    NodeID() string
    DisplayName() string
    Status() string  // "ok" | "error" | "connecting"

    // Data fetching — called by background cache loop
    FetchSessions(ctx context.Context) ([]map[string]any, error)
    FetchProjects(ctx context.Context) ([]map[string]any, error)
    FetchDiscovered(ctx context.Context) ([]map[string]any, error)
    FetchEvents(ctx context.Context, key string, after int64) ([]cli.EventEntry, error)
    Send(ctx context.Context, key, text string) error

    // Proxy operations
    ProxyTakeover(ctx context.Context, pid int, sessionID, cwd string, procStart uint64) error
    ProxyRestartPlanner(ctx context.Context, projectName string) error
    ProxyUpdateConfig(ctx context.Context, projectName string, cfg json.RawMessage) error

    // Event subscription — called by Hub (replaces wsRelay in Hub)
    Subscribe(c *wsClient, key string, after int64)
    Unsubscribe(c *wsClient, key string)
    RemoveClient(c *wsClient)

    Close()
}
```

### NodeClient adaptation

`NodeClient` already implements the data-fetching and proxy methods. Add `Subscribe`/`Unsubscribe`/
`RemoveClient`/`Close` by moving the `wsRelay` from Hub into NodeClient:

```go
type NodeClient struct {
    // existing fields...
    relayMu sync.Mutex
    relay   *wsRelay  // lazy-created on first Subscribe
}

func (n *NodeClient) NodeID() string      { return n.ID }
func (n *NodeClient) DisplayName() string { return n.DisplayName }
func (n *NodeClient) Status() string      { /* check httpClient reachability */ }

func (n *NodeClient) Subscribe(c *wsClient, key string, after int64) {
    n.relayMu.Lock()
    if n.relay == nil {
        n.relay = newWSRelay(n)
    }
    n.relayMu.Unlock()
    n.relay.Subscribe(c, key, after)
}
func (n *NodeClient) Unsubscribe(c *wsClient, key string) { n.relay?.Unsubscribe(c, key) }
func (n *NodeClient) RemoveClient(c *wsClient)             { n.relay?.RemoveClient(c) }
func (n *NodeClient) Close()                               { n.relay?.Close() }
```

---

## Primary Side: ReverseNodeConn

Represents one registered reverse connection on the primary. Created by ReverseNodeServer when a
remote connects.

```go
// internal/server/reverseconn.go

type ReverseNodeConn struct {
    id          string
    displayName string

    writeMu sync.Mutex
    conn    *websocket.Conn

    // RPC pending map
    pendingMu sync.Mutex
    pending   map[string]chan reverseResult  // req_id → waiting caller

    // Event subscriptions
    subMu sync.Mutex
    subs  map[string][]*wsClient   // key → local browser clients

    statusMu sync.RWMutex
    status   string  // "ok" | "connecting" | "error"

    done   chan struct{}
    closed bool
}
```

**RPC call** (used by FetchSessions, Send, etc.):

```go
func (c *ReverseNodeConn) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
    reqID := newReqID()
    ch := make(chan reverseResult, 1)
    c.pendingMu.Lock()
    c.pending[reqID] = ch
    c.pendingMu.Unlock()

    c.writeJSON(ReverseMsg{Type: "request", ReqID: reqID, Method: method, Params: marshal(params)})

    select {
    case res := <-ch:
        return res.result, res.err
    case <-ctx.Done():
        // Must remove from pending: if the response arrives after timeout,
        // readLoop would send to a channel nobody is receiving from (goroutine leak).
        c.pendingMu.Lock()
        delete(c.pending, reqID)
        c.pendingMu.Unlock()
        return nil, ctx.Err()
    }
}
```

**readLoop** dispatches incoming messages:

```go
func (c *ReverseNodeConn) readLoop() {
    for {
        var msg ReverseMsg
        if err := c.conn.ReadJSON(&msg); err != nil { break }

        switch msg.Type {
        case "response":
            // resolve pending RPC
            c.pendingMu.Lock()
            ch := c.pending[msg.ReqID]
            delete(c.pending, msg.ReqID)
            c.pendingMu.Unlock()
            if ch != nil { ch <- reverseResult{msg.Result, parseError(msg.Error)} }

        case "event":
            // fan out to subscribed local clients
            c.subMu.Lock()
            clients := slices.Clone(c.subs[msg.Key])
            c.subMu.Unlock()
            for _, cl := range clients {
                cl.sendJSON(wsServerMsg{Type: "event", Key: msg.Key, Event: msg.Event, Node: c.id})
            }

        case "session_state":
            c.subMu.Lock()
            clients := slices.Clone(c.subs[msg.Key])
            c.subMu.Unlock()
            for _, cl := range clients {
                cl.sendJSON(wsServerMsg{Type: "session_state", Key: msg.Key, State: msg.State, Node: c.id})
            }

        case "sessions_changed":
            // Phase 4: signal hub to refresh cache for this node
        }
    }
    c.markDisconnected()
}
```

**Subscribe/Unsubscribe** (implements NodeConn):

```go
func (c *ReverseNodeConn) Subscribe(cl *wsClient, key string, after int64) {
    c.subMu.Lock()
    alreadySub := len(c.subs[key]) > 0
    c.subs[key] = append(c.subs[key], cl)
    // Decide inside the lock to avoid check-then-act race:
    // two concurrent Subscribe calls for the same new key must only send one
    // subscribe message to the remote, not two.
    if !alreadySub {
        c.subMu.Unlock()
        c.writeJSON(ReverseMsg{Type: "subscribe", Key: key, After: after})
    } else {
        c.subMu.Unlock()
        go c.sendHistoryToClient(cl, key, after)
    }
}

// readLoop also handles subscribe_error:
//   case "subscribe_error":
//       c.subMu.Lock()
//       clients := slices.Clone(c.subs[msg.Key])
//       delete(c.subs, msg.Key)
//       c.subMu.Unlock()
//       for _, cl := range clients {
//           cl.sendJSON(wsServerMsg{Type: "error", Key: msg.Key, Node: c.id, Error: msg.Error})
//       }
```

---

## Primary Side: ReverseNodeServer

Accepts `/ws-node` connections, validates token, creates `ReverseNodeConn`.

**Security note:** `/ws-node` is an unauthenticated HTTP upgrade endpoint. An attacker can probe
it to brute-force node tokens. Mitigations:
- Return a generic `"auth failed"` error regardless of whether the node_id exists (avoid enumeration)
- Add per-IP rate limiting (e.g. `golang.org/x/time/rate`: 5 attempts / 10s per IP)

```go
// internal/server/reverseserver.go

type ReverseNodeServer struct {
    mu      sync.RWMutex
    auth    map[string]string        // node_id → expected token (from reverse_nodes config)
    conns   map[string]*ReverseNodeConn
    limiter *rate.Limiter            // or per-IP map for finer control

    onRegister   func(id string, conn *ReverseNodeConn)
    onDeregister func(id string)
}

func (s *ReverseNodeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // Rate limit by IP before upgrading
    if !s.allowIP(r.RemoteAddr) {
        http.Error(w, "too many requests", http.StatusTooManyRequests)
        return
    }

    conn, _ := wsUpgrader.Upgrade(w, r, nil)

    // 1. Read register message (5s timeout)
    var msg ReverseMsg
    conn.SetReadDeadline(time.Now().Add(5 * time.Second))
    conn.ReadJSON(&msg)
    conn.SetReadDeadline(time.Time{})

    if msg.Type != "register" { conn.Close(); return }

    // 2. Validate token — use generic error to avoid node ID enumeration
    if !s.validateToken(msg.NodeID, msg.Token) {
        slog.Warn("reverse node auth failed", "ip", r.RemoteAddr)
        conn.WriteJSON(ReverseMsg{Type: "register_fail", Error: "auth failed"})
        conn.Close()
        return
    }

    // 3. Create conn, start read loop
    rc := newReverseNodeConn(msg.NodeID, msg.DisplayName, conn)
    conn.WriteJSON(ReverseMsg{Type: "registered"})

    s.mu.Lock()
    if old, ok := s.conns[msg.NodeID]; ok { old.Close() }  // replace stale conn
    s.conns[msg.NodeID] = rc
    s.mu.Unlock()

    s.onRegister(msg.NodeID, rc)
    go rc.readLoop()
    go func() { <-rc.done; s.onDeregister(msg.NodeID) }()
    // ServeHTTP returns here; rc.readLoop keeps the WS connection alive on its goroutine.
}

func (s *ReverseNodeServer) Get(id string) *ReverseNodeConn { ... }
func (s *ReverseNodeServer) All() map[string]*ReverseNodeConn { ... }

---

## Remote Side: Connector

Lives in a new package `internal/connector`. Runs when `upstream` is configured.

```go
// internal/connector/connector.go

type Connector struct {
    cfg     config.UpstreamConfig
    router  *session.Router
    projMgr *project.Manager
    scanner *discovery.Scanner  // for fetch_discovered
}

func (c *Connector) Run(ctx context.Context) {
    backoff := time.Second
    for {
        err := c.runOnce(ctx)
        if err != nil {
            slog.Warn("connector disconnected", "err", err)
        } else {
            backoff = time.Second  // clean exit (e.g. ctx cancelled mid-session): reset
        }
        select {
        case <-ctx.Done(): return
        case <-time.After(backoff):
            backoff = min(backoff*2, 30*time.Second)
        }
    }
}

func (c *Connector) runOnce(ctx context.Context) error {
    conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.cfg.URL, nil)
    if err != nil { return err }
    defer conn.Close()

    // Register
    conn.WriteJSON(ReverseMsg{
        Type: "register", NodeID: c.cfg.NodeID,
        Token: c.cfg.Token, DisplayName: c.cfg.DisplayName,
    })
    var ack ReverseMsg
    conn.SetReadDeadline(time.Now().Add(5 * time.Second))
    conn.ReadJSON(&ack)
    conn.SetReadDeadline(time.Time{})
    if ack.Type != "registered" { return fmt.Errorf("register failed: %s", ack.Error) }

    slog.Info("connected to primary", "url", c.cfg.URL, "node_id", c.cfg.NodeID)
    return c.handleConn(ctx, conn)
    // Returning nil here resets backoff in Run().
}

func (c *Connector) handleConn(ctx context.Context, conn *websocket.Conn) error {
    // Track which keys the primary has subscribed to
    activeSubs := map[string]func(){}  // key → cancel func from local session subscription

    // WaitGroup ensures all streamEvents goroutines finish before we return,
    // so no goroutine writes to conn after defer conn.Close() in runOnce.
    var wg sync.WaitGroup
    defer wg.Wait()

    for {
        var msg ReverseMsg
        if err := conn.ReadJSON(&msg); err != nil { return err }

        switch msg.Type {
        case "request":
            go func(req ReverseMsg) {
                result, err := c.handleRequest(ctx, req)
                resp := ReverseMsg{Type: "response", ReqID: req.ReqID}
                if err != nil { resp.Error = err.Error() } else { resp.Result = result }
                conn.WriteJSON(resp)  // writeJSON is concurrency-safe with writeMu
            }(msg)

        case "subscribe":
            if _, ok := activeSubs[msg.Key]; ok { break }  // already subscribed
            sess := c.router.GetSession(msg.Key)
            if sess == nil {
                conn.WriteJSON(ReverseMsg{Type: "subscribe_error", Key: msg.Key, Error: "session not found"})
                break
            }
            notify, cancel := sess.SubscribeEvents()
            activeSubs[msg.Key] = cancel
            conn.WriteJSON(ReverseMsg{Type: "subscribed", Key: msg.Key})
            wg.Add(1)
            go func(key string, n <-chan struct{}) {
                defer wg.Done()
                c.streamEvents(ctx, conn, key, n)
            }(msg.Key, notify)

        case "unsubscribe":
            if cancel, ok := activeSubs[msg.Key]; ok {
                cancel()
                delete(activeSubs, msg.Key)
            }
            conn.WriteJSON(ReverseMsg{Type: "unsubscribed", Key: msg.Key})

        case "ping":
            conn.WriteJSON(ReverseMsg{Type: "pong"})
        }
    }
}

func (c *Connector) handleRequest(ctx context.Context, req ReverseMsg) (json.RawMessage, error) {
    switch req.Method {
    case "fetch_sessions":
        return marshalResult(c.router.ListSessions())
    case "fetch_projects":
        if c.projMgr == nil { return marshalResult([]any{}) }
        return marshalResult(c.projMgr.All())
    case "fetch_discovered":
        if c.scanner == nil { return marshalResult([]any{}) }
        return marshalResult(c.scanner.Scan())
    case "fetch_events":
        var p struct { Key string; After int64 }
        json.Unmarshal(req.Params, &p)
        sess := c.router.GetSession(p.Key)
        if sess == nil { return nil, fmt.Errorf("session not found") }
        return marshalResult(sess.EventEntriesSince(p.After))
    case "send":
        var p struct { Key, Text string }
        json.Unmarshal(req.Params, &p)
        sess, _, err := c.router.GetOrCreate(ctx, p.Key, session.AgentOpts{})
        if err != nil { return nil, err }
        go sess.Send(ctx, p.Text, nil, nil)
        // onEvent is nil: live events reach the browser only if it is already
        // subscribed to this session key. The primary must subscribe before sending
        // (Hub.handleSend already does this for local sessions; replicate for remote).
        return marshalResult(map[string]string{"status": "accepted"})
    // takeover, restart_planner, update_config...
    }
    return nil, fmt.Errorf("unknown method: %s", req.Method)
}

func (c *Connector) streamEvents(ctx context.Context, conn *websocket.Conn, key string, notify <-chan struct{}) {
    sess := c.router.GetSession(key)
    if sess == nil { return }
    var lastTime int64
    for {
        select {
        case _, ok := <-notify:
            if !ok { return }
            for _, ev := range sess.EventEntriesSince(lastTime) {
                conn.WriteJSON(ReverseMsg{Type: "event", Key: key, Event: &ev})
                if ev.Time > lastTime { lastTime = ev.Time }
            }
        case <-ctx.Done():
            return
        }
    }
}
```

---

## Server / Hub Wiring Changes

### Server.nodes: `*NodeClient` → `NodeConn`

```go
// server.go
nodes map[string]NodeConn  // was: map[string]*NodeClient

func (s *Server) SetNodes(nodes map[string]NodeConn) { s.nodes = nodes }

func (s *Server) SetReverseNodeServer(rns *ReverseNodeServer) {
    rns.onRegister = func(id string, rc *ReverseNodeConn) {
        s.nodesMu.Lock()
        s.nodes[id] = rc
        s.nodesMu.Unlock()
        go s.refreshNodeCacheFor(id)         // immediate cache refresh on connect
        s.hub.NotifyNodeChange()
    }
    rns.onDeregister = func(id string) {
        s.nodesMu.Lock()
        delete(s.nodes, id)
        s.nodesMu.Unlock()
        // Purge cached data for disconnected node so /api/sessions doesn't
        // serve stale sessions with no connection to service them.
        s.nodeCacheMu.Lock()
        delete(s.nodeSessions, id)
        delete(s.nodeProjects, id)
        delete(s.nodeDiscovered, id)
        s.nodeStatus[id] = "error"
        s.nodeCacheMu.Unlock()
        s.hub.NotifyNodeChange()
        s.hub.BroadcastSessionsUpdate()
    }
}

// refreshNodeCacheFor fetches and caches data for a single node immediately.
// Used on reverse-node connect and on sessions_changed push (Phase 4).
func (s *Server) refreshNodeCacheFor(id string) {
    s.nodesMu.RLock()
    nc, ok := s.nodes[id]
    s.nodesMu.RUnlock()
    if !ok { return }

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    var wg sync.WaitGroup
    var sessions, projects, discovered []map[string]any
    wg.Add(3)
    go func() { defer wg.Done(); sessions, _ = nc.FetchSessions(ctx) }()
    go func() { defer wg.Done(); projects, _ = nc.FetchProjects(ctx) }()
    go func() { defer wg.Done(); discovered, _ = nc.FetchDiscovered(ctx) }()
    wg.Wait()

    for _, rs := range sessions   { rs["node"] = id }
    for _, rp := range projects   { rp["node"] = id }
    for _, rd := range discovered { rd["node"] = id }

    s.nodeCacheMu.Lock()
    s.nodeSessions[id]   = sessions
    s.nodeProjects[id]   = projects
    s.nodeDiscovered[id] = discovered
    s.nodeStatus[id]     = "ok"
    s.nodeCacheMu.Unlock()

    s.hub.BroadcastSessionsUpdate()
}
```

`refreshNodeCache` is unchanged — calls `nc.FetchSessions()` etc., works for both NodeConn impls.

### Hub: remove `relays` map

```go
// wshub.go
type Hub struct {
    // remove: relays  map[string]*wsRelay
    nodes map[string]NodeConn  // was: *NodeClient
}

func (h *Hub) handleRemoteSubscribe(c *wsClient, msg wsClientMsg) {
    conn := h.nodes[msg.Node]          // NodeConn, not *NodeClient
    conn.Subscribe(c, msg.Key, msg.After)
}

func (h *Hub) handleRemoteUnsubscribe(c *wsClient, msg wsClientMsg) {
    conn := h.nodes[msg.Node]
    conn.Unsubscribe(c, msg.Key)
}

func (h *Hub) unregister(c *wsClient) {
    // same as before, but call conn.RemoveClient(c) instead of relay.RemoveClient(c)
    for _, conn := range h.nodes {
        conn.RemoveClient(c)
    }
}

func (h *Hub) Shutdown() {
    for _, conn := range h.nodes { conn.Close() }
    // remove relay close loop
}
```

**Node disconnect cleanup:** When a reverse node deregisters, browser clients with active
subscriptions to that node's sessions must be notified. `onDeregister` calls
`BroadcastSessionsUpdate` which causes browsers to re-fetch `/api/sessions`. The deleted node's
sessions are no longer in the response. Browsers that were subscribed to those sessions will have
stale subscriptions until they navigate away or refresh. To handle this explicitly:

```go
// Called by onDeregister after deleting from s.nodes
func (h *Hub) PurgeNodeSubscriptions(nodeID string) {
    // ReverseNodeConn.Close() already stops further event delivery.
    // Notify subscribed clients that the session is gone.
    h.mu.Lock()
    defer h.mu.Unlock()
    for c := range h.clients {
        c.sendJSON(wsServerMsg{
            Type: "error", Node: nodeID,
            Error: "node disconnected",
        })
    }
}
```

This is a best-effort notification; browsers treat unknown error messages as a signal to
re-subscribe or deselect the session.

---

## Config Changes

### Primary: `reverse_nodes` (new, for reverse connections)

```yaml
# config.yaml — primary node

workspace:
  id: "ec2-prod"
  name: "EC2 Prod"

# Existing: direct-reach nodes (unchanged)
workspaces:
  dev-server:
    url: "http://10.0.0.5:8180"
    token: "secret"
    display_name: "Dev Server"

# New: pre-authorize reverse-connecting nodes
# Naming mirrors 'workspaces': same concept, opposite connection direction.
reverse_nodes:
  macbook:
    token: "macbook-secret-xxx"
    display_name: "MacBook Pro"
  home-desktop:
    token: "desktop-secret-xxx"
    display_name: "Home Desktop"
```

`reverse_nodes` entries have no `url` — primary waits for them to dial in.

### Remote: `upstream` (new)

```yaml
# config.yaml — remote (MacBook)

workspace:
  id: "macbook"
  name: "MacBook Pro"

upstream:
  url: "wss://ec2.example.com:8180/ws-node"   # or ws:// for plain
  node_id: "macbook"                           # must match reverse_nodes key on primary
  token: "macbook-secret-xxx"                  # must match reverse_nodes[macbook].token
  display_name: "MacBook Pro"                  # shown in dashboard (can override)
```

### Config struct additions

```go
type Config struct {
    // existing...
    ReverseNodes map[string]ReverseNodeEntry `yaml:"reverse_nodes"`  // primary: accept inbound
    Upstream     *UpstreamConfig             `yaml:"upstream"`       // remote: dial primary
}

type ReverseNodeEntry struct {
    Token       string `yaml:"token"`
    DisplayName string `yaml:"display_name"`
    // Remote's display_name in register message takes precedence if non-empty.
}

type UpstreamConfig struct {
    URL         string `yaml:"url"`
    NodeID      string `yaml:"node_id"`
    Token       string `yaml:"token"`
    DisplayName string `yaml:"display_name"`
}
```

---

## File Change Map

| File | Change | ~Lines |
|------|--------|--------|
| `internal/reverse/proto.go` | **New**: ReverseMsg type (shared by server + connector) | +40 |
| `internal/server/nodeconn.go` | **New**: NodeConn interface | +30 |
| `internal/server/reverseconn.go` | **New**: ReverseNodeConn (primary-side) | +280 |
| `internal/server/reverseconn_test.go` | **New**: RPC + event push tests | +200 |
| `internal/server/reverseserver.go` | **New**: ReverseNodeServer, /ws-node handler, rate limit | +130 |
| `internal/connector/connector.go` | **New**: remote-side Connector | +260 |
| `internal/connector/connector_test.go` | **New**: Connector request handling tests | +150 |
| `internal/config/config.go` | Add ReverseNodes, UpstreamConfig, validation | +50 |
| `internal/server/nodeclient.go` | Add Subscribe/Unsubscribe/RemoveClient/Close/NodeID | +60 |
| `internal/server/wshub.go` | NodeConn instead of *NodeClient, remove relays, PurgeNodeSubscriptions | −30/+25 |
| `internal/server/wsrelay.go` | Remove unused `hub *Hub` field | −5 |
| `internal/server/server.go` | NodeConn, ReverseNodeServer wiring, nodesMu, refreshNodeCacheFor | +80 |
| `cmd/naozhi/main.go` | Build reverse_nodes → ReverseNodeServer; start Connector | +40 |
| `internal/server/dashboard.go` | No change (uses NodeConn via server.nodes) | 0 |
| **Total** | | **~1310** |

---

## Implementation Phases

### Phase 1 — NodeConn abstraction (no behavior change)

1. Define `NodeConn` interface in `nodeconn.go`
2. Add `Subscribe`/`Unsubscribe`/`RemoveClient`/`Close` to `NodeClient` (wraps existing wsRelay)
3. Move wsRelay from Hub into NodeClient (Hub.relays → NodeClient.relay)
4. Change `Hub.nodes`, `Server.nodes` to `map[string]NodeConn`
5. Update Hub.handleRemoteSubscribe/Unsubscribe/unregister/Shutdown to call NodeConn methods
6. **All existing tests pass unchanged** — behavior identical, only internals moved

### Phase 2 — ReverseNodeConn + ReverseNodeServer (primary side)

1. `reverseproto.go`: ReverseMsg struct
2. `reverseconn.go`: ReverseNodeConn with RPC multiplexing + event subscription
3. `reverseserver.go`: accept /ws-node, validate token, create ReverseNodeConn
4. Wire into Server: `SetReverseNodeServer`, register /ws-node route
5. Config: parse `reverse_nodes` → pass to ReverseNodeServer
6. Tests: mock Connector dials mock ReverseNodeServer, verify RPC + push

### Phase 3 — Connector (remote side)

1. `internal/connector/connector.go`: dial, register, handle requests, stream events
2. Config: parse `upstream` → create Connector in main.go
3. Start `connector.Run(ctx)` as goroutine in main
4. Tests: mock primary WS, verify Connector handles fetch_sessions, send, subscribe

### Phase 4 — Proactive push (optional)

1. Remote sends `sessions_changed` when Router.SetOnChange fires
2. Primary calls `refreshNodeCacheFor(id)` immediately on receiving it
3. Hub broadcasts sessions_update to all browser clients
4. Effect: dashboard sidebar updates in ~100ms instead of up to 10s

---

## Verification

### Unit tests

```
reverseconn_test.go:
  TestReverseNodeConn_RPC                    — request/response multiplexing
  TestReverseNodeConn_ConcurrentRPC          — multiple in-flight requests
  TestReverseNodeConn_RPCTimeout             — ctx cancelled before response
  TestReverseNodeConn_Subscribe              — subscribe message sent to remote
  TestReverseNodeConn_EventPush              — events forwarded to local clients
  TestReverseNodeConn_ReconnectResubs        — resubscribes after reconnect

connector_test.go:
  TestConnector_Register                     — registration handshake
  TestConnector_RegisterBadToken             — rejected token
  TestConnector_FetchSessions                — handles fetch_sessions request
  TestConnector_Send                         — handles send request
  TestConnector_Subscribe                    — starts streaming events
  TestConnector_Reconnect                    — reconnects after primary disconnect
```

### Integration test — two processes, same machine

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

```bash
./naozhi --config config-primary.yaml &
./naozhi --config config-remote.yaml &

sleep 2  # wait for reverse connection

# Remote should appear in primary's nodes
curl -s http://localhost:8180/api/sessions | jq '.nodes'
# {"local":{...},"laptop":{"status":"ok","display_name":"Laptop"}}

# Sessions from remote appear in aggregated list (after first 10s cache cycle)
# OR immediately after sessions_changed push (Phase 4)
curl -s http://localhost:8180/api/sessions | jq '.sessions[].node'

# Send from primary to remote session
curl -s -X POST http://localhost:8180/api/sessions/send \
  -H "Content-Type: application/json" \
  -d '{"key":"dashboard:d:test:general","text":"hello","node":"laptop"}'

# Reconnect: kill primary, restart — remote should reconnect and re-register
```

### Dashboard UI checklist

- [ ] Remote node appears in sidebar with green dot immediately on connect
- [ ] Red dot appears when remote disconnects (within one cache cycle or via WS push)
- [ ] Clicking remote session subscribes via reverse WS (events appear live)
- [ ] Sending from dashboard routes through reverse channel to remote CLI
- [ ] Remote node behind NAT works (test with actual different-network machine)

---

## Compatibility

- **Direct nodes** (`workspaces` config) are unchanged — NodeClient still works as before
- Both modes can coexist: one EC2 in `workspaces` (direct), one MacBook in `reverse_nodes` (reverse)
- Remote nodes running old naozhi (no upstream config) remain usable via direct connect if reachable
- Remote nodes on NAT use new upstream config — no other changes needed on remote
