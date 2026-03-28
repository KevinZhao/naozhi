package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// NodeClient is an HTTP client for a remote naozhi instance.
type NodeClient struct {
	ID          string
	URL         string // e.g. "http://10.0.0.2:8180"
	Token       string // dashboard bearer token
	DisplayName string
	httpClient  *http.Client
}

// NewNodeClient creates a NodeClient with a 10s timeout.
func NewNodeClient(id, url, token, displayName string) *NodeClient {
	return &NodeClient{
		ID:          id,
		URL:         url,
		Token:       token,
		DisplayName: displayName,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (n *NodeClient) doRequest(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
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
func (n *NodeClient) FetchSessions(ctx context.Context) ([]map[string]any, error) {
	resp, err := n.doRequest(ctx, http.MethodGet, "/api/sessions", nil)
	if err != nil {
		return nil, fmt.Errorf("fetch sessions from %s: %w", n.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch sessions from %s: status %d", n.ID, resp.StatusCode)
	}

	var result struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode sessions from %s: %w", n.ID, err)
	}
	return result.Sessions, nil
}

// FetchEvents fetches event entries from the remote node via GET /api/sessions/events.
func (n *NodeClient) FetchEvents(ctx context.Context, key string, after int64) ([]cli.EventEntry, error) {
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
		return nil, fmt.Errorf("fetch events from %s: status %d", n.ID, resp.StatusCode)
	}

	var entries []cli.EventEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode events from %s: %w", n.ID, err)
	}
	return entries, nil
}

// Send sends a message to a session on the remote node via POST /api/sessions/send.
func (n *NodeClient) Send(ctx context.Context, key, text string) error {
	payload := map[string]string{"key": key, "text": text}
	data, _ := json.Marshal(payload)
	resp, err := n.doRequest(ctx, http.MethodPost, "/api/sessions/send", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("send to %s: %w", n.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("send to %s: status %d", n.ID, resp.StatusCode)
	}
	return nil
}

// Health checks the remote node's health via GET /health.
func (n *NodeClient) Health(ctx context.Context) error {
	resp, err := n.doRequest(ctx, http.MethodGet, "/health", nil)
	if err != nil {
		return fmt.Errorf("health check %s: %w", n.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check %s: status %d", n.ID, resp.StatusCode)
	}

	var result struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("health check %s: decode: %w", n.ID, err)
	}
	if result.Status != "ok" {
		return fmt.Errorf("health check %s: status is %q", n.ID, result.Status)
	}
	return nil
}
