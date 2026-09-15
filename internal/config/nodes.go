package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

// validateNodes checks the remote-node, upstream and reverse-node blocks. A
// plaintext-http node carrying a bearer token is refused outright; without a
// token it needs an explicit insecure opt-in. Split out of validateConfig (#2710).
func validateNodes(cfg *Config) error {
	for id, nc := range cfg.Nodes {
		if nc.URL == "" {
			return fmt.Errorf("node %q: url is required", id)
		}
		if strings.HasSuffix(nc.URL, "/") {
			return fmt.Errorf("node %q: url must not have trailing slash", id)
		}
		u, err := url.Parse(nc.URL)
		if err != nil {
			return fmt.Errorf("node %q: invalid url: %w", id, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("node %q: url must be http or https", id)
		}
		if u.Scheme == "http" && nc.Token != "" {
			return fmt.Errorf("node %q: refusing to send bearer token over plaintext HTTP — use HTTPS", id)
		}
		if u.Scheme == "http" && nc.Token == "" {
			if !nc.Insecure {
				return fmt.Errorf("node %q: plaintext HTTP without authentication is unsafe — set insecure: true to allow", id)
			}
			slog.Warn("node uses plaintext HTTP without authentication — session data is exposed to network attackers", "node", id)
		}
	}

	if cfg.Upstream != nil {
		if cfg.Upstream.URL == "" {
			return fmt.Errorf("upstream.url is required")
		}
		if containsEnvPlaceholder(cfg.Upstream.URL) {
			return fmt.Errorf("upstream.url contains unexpanded ${VAR} — check environment variables")
		}
		if !strings.HasPrefix(cfg.Upstream.URL, "wss://") && !strings.HasPrefix(cfg.Upstream.URL, "ws://") {
			return fmt.Errorf("upstream.url must use ws:// or wss:// scheme")
		}
		if strings.HasPrefix(cfg.Upstream.URL, "ws://") && !cfg.Upstream.Insecure {
			return fmt.Errorf("upstream.url must use wss:// — refusing to send bearer token over plaintext ws:// (set insecure: true to allow)")
		}
		if cfg.Upstream.NodeID == "" {
			return fmt.Errorf("upstream.node_id is required")
		}
		if containsEnvPlaceholder(cfg.Upstream.NodeID) {
			return fmt.Errorf("upstream.node_id contains unexpanded ${VAR} — check environment variables")
		}
		if cfg.Upstream.Token == "" {
			return fmt.Errorf("upstream.token is required")
		}
		if containsEnvPlaceholder(cfg.Upstream.Token) {
			return fmt.Errorf("upstream.token contains unexpanded ${VAR} — check environment variables")
		}
		if cfg.Upstream.Token == "your-secret-token" {
			return fmt.Errorf("upstream.token is set to the example placeholder \"your-secret-token\" — replace it with a real secret")
		}
	}

	for id, entry := range cfg.ReverseNodes {
		if entry.Token == "" {
			return fmt.Errorf("reverse_nodes %q: token is required", id)
		}
		if containsEnvPlaceholder(entry.Token) {
			return fmt.Errorf("reverse_nodes %q: token contains unexpanded ${VAR} — check environment variables", id)
		}
	}
	return nil
}
