package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// HTTPClient is an HTTP client for a remote naozhi instance.
type HTTPClient struct {
	ID          string
	URL         string // e.g. "http://10.0.0.2:8180"; cleaned by validatePeerURL
	Token       string // dashboard bearer token
	displayName string
	httpClient  *http.Client

	// urlErr is non-nil when the URL failed validatePeerURL; doRequest
	// short-circuits so a bearer-token request never leaves toward an
	// unvalidated target (#1548).
	urlErr error

	// localPeer marks a loopback / private literal-IP peer. Allowed (documented
	// LAN topology), but a config-write attacker could repoint it at a
	// co-located service (Redis 6379, Elasticsearch 9200, Docker 2375) and use
	// the request body as an SSRF write primitive, so doRequest caps the body
	// for such peers (#1825).
	localPeer bool

	relayMu sync.Mutex
	relay   *wsRelay
}

// maxLocalPeerBodyBytes caps request bodies to loopback/private peers; real
// proxy payloads are small JSON, public peers stay uncapped (#1825).
const maxLocalPeerBodyBytes = 1 << 20 // 1 MiB

// NewHTTPClient creates an HTTPClient with a 10s timeout. An invalid/unsafe
// peer URL (validatePeerURL) does not panic: it is recorded and every request
// fails cleanly, so a tampered config cannot turn the token into an SSRF probe.
func NewHTTPClient(id, rawURL, token, displayName string) *HTTPClient {
	cleanURL, urlErr := validatePeerURL(rawURL)
	if urlErr != nil {
		// Kept for diagnostics/RemoteAddr only; doRequest refuses to use it.
		cleanURL = strings.TrimSpace(rawURL)
		slog.Error("node peer URL rejected; client disabled",
			"node", id, "url", rawURL, "err", urlErr)
	}
	localPeer := urlErr == nil && isLocalPeerURL(cleanURL)
	return &HTTPClient{
		ID:          id,
		URL:         cleanURL,
		Token:       token,
		displayName: displayName,
		urlErr:      urlErr,
		localPeer:   localPeer,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        30,
				MaxIdleConnsPerHost: 6,
				IdleConnTimeout:     90 * time.Second,
				// Screens the RESOLVED IP: a hostname rebinding to IMDS would
				// otherwise carry the bearer token there (#1677).
				DialContext: safeDialContext,
				// Pin the TLS floor.
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			},
			// A 3xx from a compromised peer must not redirect the bearer token
			// to IMDS or an internal address.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (n *HTTPClient) doRequest(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	if n.urlErr != nil {
		return nil, fmt.Errorf("node %s: refusing request to unvalidated peer URL: %w", n.ID, n.urlErr)
	}
	req, err := http.NewRequestWithContext(ctx, method, n.URL+path, body)
	if err != nil {
		return nil, err
	}
	// All callers pass *bytes.Reader, so ContentLength is exact here.
	if n.localPeer && req.ContentLength > maxLocalPeerBodyBytes {
		return nil, fmt.Errorf("node %s: request body %d bytes exceeds %d-byte cap for loopback/private peer (SSRF-write guard)",
			n.ID, req.ContentLength, maxLocalPeerBodyBytes)
	}
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return n.httpClient.Do(req)
}

// FetchSessions fetches sessions from the remote node via GET /api/sessions.
func (n *HTTPClient) FetchSessions(ctx context.Context) ([]map[string]any, error) {
	resp, err := n.doRequest(ctx, http.MethodGet, "/api/sessions", nil)
	if err != nil {
		return nil, fmt.Errorf("fetch sessions from %s: %w", n.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("fetch sessions from %s: status %d", n.ID, resp.StatusCode)
	}

	var result struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode sessions from %s: %w", n.ID, err)
	}
	return result.Sessions, nil
}

// FetchEvents fetches event entries from the remote node via GET /api/sessions/events.
func (n *HTTPClient) FetchEvents(ctx context.Context, key string, after int64) ([]clievent.EventEntry, error) {
	path := "/api/sessions/events?key=" + url.QueryEscape(key)
	if after > 0 {
		path += "&after=" + strconv.FormatInt(after, 10)
	}
	resp, err := n.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch events from %s: %w", n.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("fetch events from %s: status %d", n.ID, resp.StatusCode)
	}

	var entries []clievent.EventEntry
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode events from %s: %w", n.ID, err)
	}
	return entries, nil
}

// Send sends a message to a session on the remote node via POST /api/sessions/send.
func (n *HTTPClient) Send(ctx context.Context, key, text, workspace string) error {
	payload := map[string]string{"key": key, "text": text}
	if workspace != "" {
		payload["workspace"] = workspace
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal send payload: %w", err)
	}
	resp, err := n.doRequest(ctx, http.MethodPost, "/api/sessions/send", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("send to %s: %w", n.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("send to %s: status %d", n.ID, resp.StatusCode)
	}
	return nil
}

// FetchProjects fetches projects from the remote node via GET /api/projects.
func (n *HTTPClient) FetchProjects(ctx context.Context) ([]map[string]any, error) {
	resp, err := n.doRequest(ctx, http.MethodGet, "/api/projects", nil)
	if err != nil {
		return nil, fmt.Errorf("fetch projects from %s: %w", n.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("fetch projects from %s: status %d", n.ID, resp.StatusCode)
	}

	var result []map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode projects from %s: %w", n.ID, err)
	}
	return result, nil
}

// FetchDiscovered fetches discovered sessions from the remote node via GET /api/discovered.
func (n *HTTPClient) FetchDiscovered(ctx context.Context) ([]map[string]any, error) {
	resp, err := n.doRequest(ctx, http.MethodGet, "/api/discovered", nil)
	if err != nil {
		return nil, fmt.Errorf("fetch discovered from %s: %w", n.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("fetch discovered from %s: status %d", n.ID, resp.StatusCode)
	}

	var result []map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode discovered from %s: %w", n.ID, err)
	}
	return result, nil
}

// FetchDiscoveredPreview fetches conversation history for a discovered session from the remote node.
func (n *HTTPClient) FetchDiscoveredPreview(ctx context.Context, sessionID string) ([]clievent.EventEntry, error) {
	resp, err := n.doRequest(ctx, http.MethodGet, "/api/discovered/preview?session_id="+url.QueryEscape(sessionID), nil)
	if err != nil {
		return nil, fmt.Errorf("fetch discovered preview from %s: %w", n.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("fetch discovered preview from %s: status %d", n.ID, resp.StatusCode)
	}

	var result []clievent.EventEntry
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode discovered preview from %s: %w", n.ID, err)
	}
	return result, nil
}

// FetchBackends relays GET /api/cli/backends verbatim as raw JSON (see
// NodeFetcher.FetchBackends); any non-200 (incl. an older peer's 404) is an error.
func (n *HTTPClient) FetchBackends(ctx context.Context) (json.RawMessage, error) {
	resp, err := n.doRequest(ctx, http.MethodGet, "/api/cli/backends", nil)
	if err != nil {
		return nil, fmt.Errorf("fetch backends from %s: %w", n.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("fetch backends from %s: status %d", n.ID, resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read backends from %s: %w", n.ID, err)
	}
	// A compromised peer must not stream arbitrary bytes to the dashboard as JSON.
	if !json.Valid(raw) {
		return nil, fmt.Errorf("fetch backends from %s: malformed JSON body", n.ID)
	}
	return json.RawMessage(raw), nil
}

// ProxyTakeover forwards a takeover request to the remote node.
func (n *HTTPClient) ProxyTakeover(ctx context.Context, pid int, sessionID, cwd string, procStartTime uint64) (string, error) {
	payload := map[string]any{"pid": pid, "session_id": sessionID, "cwd": cwd, "proc_start_time": procStartTime}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal takeover payload: %w", err)
	}
	resp, err := n.doRequest(ctx, http.MethodPost, "/api/discovered/takeover", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("proxy takeover to %s: %w", n.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return "", fmt.Errorf("proxy takeover to %s: status %d: %s", n.ID, resp.StatusCode, string(body))
	}
	var result struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&result); err != nil {
		return "", fmt.Errorf("proxy takeover to %s: decode response: %w", n.ID, err)
	}
	return result.Key, nil
}

// ProxyCloseDiscovered forwards a close-discovered request to the remote node.
func (n *HTTPClient) ProxyCloseDiscovered(ctx context.Context, pid int, sessionID, cwd string, procStartTime uint64) error {
	payload := map[string]any{"pid": pid, "session_id": sessionID, "cwd": cwd, "proc_start_time": procStartTime}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal close discovered payload: %w", err)
	}
	resp, err := n.doRequest(ctx, http.MethodPost, "/api/discovered/close", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("proxy close discovered to %s: %w", n.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("proxy close discovered to %s: status %d: %s", n.ID, resp.StatusCode, string(body))
	}
	return nil
}

// ProxyRestartPlanner forwards a planner restart request to the remote node.
func (n *HTTPClient) ProxyRestartPlanner(ctx context.Context, projectName string) error {
	resp, err := n.doRequest(ctx, http.MethodPost, "/api/projects/planner/restart?name="+url.QueryEscape(projectName), nil)
	if err != nil {
		return fmt.Errorf("proxy restart planner to %s: %w", n.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("proxy restart planner to %s: status %d: %s", n.ID, resp.StatusCode, string(body))
	}
	return nil
}

// ProxyUpdateConfig forwards a project config update to the remote node.
func (n *HTTPClient) ProxyUpdateConfig(ctx context.Context, projectName string, cfg json.RawMessage) error {
	resp, err := n.doRequest(ctx, http.MethodPut, "/api/projects/config?name="+url.QueryEscape(projectName), bytes.NewReader(cfg))
	if err != nil {
		return fmt.Errorf("proxy update config to %s: %w", n.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("proxy update config to %s: status %d: %s", n.ID, resp.StatusCode, string(body))
	}
	return nil
}

// ProxyRemoveSession forwards DELETE /api/sessions to the remote node.
func (n *HTTPClient) ProxyRemoveSession(ctx context.Context, key string) (bool, error) {
	data, err := json.Marshal(map[string]string{"key": key})
	if err != nil {
		return false, fmt.Errorf("marshal remove session payload: %w", err)
	}
	resp, err := n.doRequest(ctx, http.MethodDelete, "/api/sessions", bytes.NewReader(data))
	if err != nil {
		return false, fmt.Errorf("proxy remove session to %s: %w", n.ID, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return true, nil
	case http.StatusNotFound:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return false, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return false, fmt.Errorf("proxy remove session to %s: status %d: %s", n.ID, resp.StatusCode, string(body))
	}
}

// ProxySetSessionLabel forwards PATCH /api/sessions/label: (true, nil) on 200,
// (false, nil) on 404 (also what an older peer without the route returns).
func (n *HTTPClient) ProxySetSessionLabel(ctx context.Context, key, label string) (bool, error) {
	data, err := json.Marshal(map[string]string{"key": key, "label": label})
	if err != nil {
		return false, fmt.Errorf("marshal set session label payload: %w", err)
	}
	resp, err := n.doRequest(ctx, http.MethodPatch, "/api/sessions/label", bytes.NewReader(data))
	if err != nil {
		return false, fmt.Errorf("proxy set session label to %s: %w", n.ID, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return true, nil
	case http.StatusNotFound:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return false, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return false, fmt.Errorf("proxy set session label to %s: status %d: %s", n.ID, resp.StatusCode, string(body))
	}
}

// ProxyInterruptSession forwards POST /api/sessions/interrupt to the remote node.
func (n *HTTPClient) ProxyInterruptSession(ctx context.Context, key string) (bool, error) {
	data, err := json.Marshal(map[string]string{"key": key})
	if err != nil {
		return false, fmt.Errorf("marshal interrupt payload: %w", err)
	}
	resp, err := n.doRequest(ctx, http.MethodPost, "/api/sessions/interrupt", bytes.NewReader(data))
	if err != nil {
		return false, fmt.Errorf("proxy interrupt session to %s: %w", n.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return false, fmt.Errorf("proxy interrupt session to %s: status %d: %s", n.ID, resp.StatusCode, string(body))
	}
	var result struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&result); err != nil {
		return false, fmt.Errorf("proxy interrupt session to %s: decode response: %w", n.ID, err)
	}
	return result.Status == "ok", nil
}

// ProxySetFavorite forwards a project favorite toggle to the remote node.
func (n *HTTPClient) ProxySetFavorite(ctx context.Context, projectName string, favorite bool) error {
	favStr := "false"
	if favorite {
		favStr = "true"
	}
	path := "/api/projects/favorite?name=" + url.QueryEscape(projectName) + "&favorite=" + favStr
	resp, err := n.doRequest(ctx, http.MethodPost, path, nil)
	if err != nil {
		return fmt.Errorf("proxy set favorite to %s: %w", n.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("proxy set favorite to %s: status %d: %s", n.ID, resp.StatusCode, string(body))
	}
	return nil
}

func (n *HTTPClient) NodeID() string      { return n.ID }
func (n *HTTPClient) DisplayName() string { return n.displayName }
func (n *HTTPClient) Status() string      { return "ok" }
func (n *HTTPClient) RemoteAddr() string  { return n.URL }

// Meta returns a NodeMeta advertising no capabilities: pull-mode peers have
// no negotiation surface, so backends with RequiredNodeCaps are denied while
// claude (nil caps) still dispatches. Allocated per call (cold path).
func (n *HTTPClient) Meta() *NodeMeta {
	return &NodeMeta{
		NodeID:      n.ID,
		DisplayName: n.displayName,
	}
}

func (n *HTTPClient) Subscribe(c EventSink, key string, after int64) {
	n.relayMu.Lock()
	if n.relay == nil {
		n.relay = newWSRelay(n)
	}
	relay := n.relay
	n.relayMu.Unlock()
	relay.Subscribe(c, key, after)
}

func (n *HTTPClient) Unsubscribe(c EventSink, key string) {
	n.relayMu.Lock()
	relay := n.relay
	n.relayMu.Unlock()
	if relay != nil {
		relay.Unsubscribe(c, key)
	}
}

// RefreshSubscription is a no-op for HTTP nodes.
func (n *HTTPClient) RefreshSubscription(key string) {}

func (n *HTTPClient) RemoveClient(c EventSink) {
	n.relayMu.Lock()
	relay := n.relay
	n.relayMu.Unlock()
	if relay != nil {
		relay.RemoveClient(c)
	}
}

func (n *HTTPClient) Close() {
	n.relayMu.Lock()
	relay := n.relay
	n.relay = nil
	n.relayMu.Unlock()
	if relay != nil {
		relay.Close()
	}
}
