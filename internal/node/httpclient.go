package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// HTTPClient is an HTTP client for a remote naozhi instance.
type HTTPClient struct {
	ID          string
	URL         string // e.g. "http://10.0.0.2:8180"
	Token       string // dashboard bearer token
	displayName string
	httpClient  *http.Client

	relayMu sync.Mutex
	relay   *wsRelay
}

// NewHTTPClient creates an HTTPClient with a 10s timeout.
func NewHTTPClient(id, url, token, displayName string) *HTTPClient {
	return &HTTPClient{
		ID:          id,
		URL:         url,
		Token:       token,
		displayName: displayName,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        30,
				MaxIdleConnsPerHost: 6,
				IdleConnTimeout:     90 * time.Second,
				// Pin a minimum TLS version so a future Go toolchain change
				// or GODEBUG override cannot silently accept TLS 1.0/1.1.
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			},
		},
	}
}

func (n *HTTPClient) doRequest(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, n.URL+path, body)
	if err != nil {
		return nil, err
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
func (n *HTTPClient) FetchEvents(ctx context.Context, key string, after int64) ([]cli.EventEntry, error) {
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

	var entries []cli.EventEntry
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
func (n *HTTPClient) FetchDiscoveredPreview(ctx context.Context, sessionID string) ([]cli.EventEntry, error) {
	resp, err := n.doRequest(ctx, http.MethodGet, "/api/discovered/preview?session_id="+url.QueryEscape(sessionID), nil)
	if err != nil {
		return nil, fmt.Errorf("fetch discovered preview from %s: %w", n.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("fetch discovered preview from %s: status %d", n.ID, resp.StatusCode)
	}

	var result []cli.EventEntry
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode discovered preview from %s: %w", n.ID, err)
	}
	return result, nil
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

// RefreshSubscription is a no-op for HTTP nodes. The wsRelay polls events
// via HTTP, so there is no persistent subscription to refresh.
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
