package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/webhook"
)

// IntegrationsConfig is the integrations block: outbound hooks the server
// calls on run lifecycle events (docs/rfc/outbound-webhooks.md).
type IntegrationsConfig struct {
	Webhooks []WebhookConfig `yaml:"webhooks,omitempty"`
}

// WebhookConfig is one receiver.
type WebhookConfig struct {
	URL string `yaml:"url"`
	// Secret, when set, signs each body (X-Naozhi-Signature: sha256=…).
	Secret string `yaml:"secret,omitempty"`
	// Events ⊆ {run.started, run.ended}; empty = both.
	Events []string `yaml:"events,omitempty"`
	// Subsystems ⊆ {cron, sysession}; empty = both.
	Subsystems []string `yaml:"subsystems,omitempty"`
	// Timeout per HTTP attempt, e.g. "10s"; empty = webhook.DefaultTimeout.
	Timeout string `yaml:"timeout,omitempty"`
}

// LogValue implements slog.LogValuer: the URL is reduced to scheme://host
// (its path or query may carry a token) and the secret is redacted.
func (c WebhookConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("url", webhook.RedactURL(strings.TrimSpace(c.URL))),
		slog.String("secret", redactSecret(c.Secret)),
		slog.Any("events", c.Events),
		slog.Any("subsystems", c.Subsystems),
		slog.String("timeout", c.Timeout),
	)
}

var (
	webhookEvents     = []string{webhook.EventRunStarted, webhook.EventRunEnded}
	webhookSubsystems = []string{"cron", "sysession"}
)

// validateIntegrations rejects a webhook the sender could not honour.
func validateIntegrations(cfg *Config) error {
	for i, w := range cfg.Integrations.Webhooks {
		where := fmt.Sprintf("integrations.webhooks[%d]", i)
		u, err := url.Parse(strings.TrimSpace(w.URL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s.url must be an absolute http(s) URL", where)
		}
		if containsEnvPlaceholder(w.URL) || containsEnvPlaceholder(w.Secret) {
			return fmt.Errorf("%s contains unexpanded ${VAR} — check environment variables", where)
		}
		for _, e := range w.Events {
			if !slices.Contains(webhookEvents, e) {
				return fmt.Errorf("%s.events: unknown event %q (want %s)", where, e, strings.Join(webhookEvents, ", "))
			}
		}
		for _, s := range w.Subsystems {
			if !slices.Contains(webhookSubsystems, s) {
				return fmt.Errorf("%s.subsystems: unknown subsystem %q (want %s)", where, s, strings.Join(webhookSubsystems, ", "))
			}
		}
		if w.Timeout != "" {
			if d, err := time.ParseDuration(w.Timeout); err != nil || d <= 0 {
				return fmt.Errorf("%s.timeout: invalid duration %q", where, w.Timeout)
			}
		}
	}
	return nil
}

// integrationsDiags warns about a signed webhook sent over plain http to a
// non-loopback host: the signature travels in the clear.
func (c *Config) integrationsDiags() []ValidationDiag {
	var out []ValidationDiag
	for i, w := range c.Integrations.Webhooks {
		u, err := url.Parse(w.URL)
		if err != nil || u.Scheme != "http" {
			continue
		}
		host := u.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
			continue
		}
		out = append(out, ValidationDiag{
			Level: "warn",
			Field: fmt.Sprintf("integrations.webhooks[%d].url", i),
			Msg:   "webhook uses plain http to a non-loopback host; the body and its signature travel unencrypted",
			Hint:  "use https, or accept this for a trusted private network",
		})
	}
	return out
}

// WebhookEndpoints converts the block into the sender's endpoints.
func (c *Config) WebhookEndpoints() []webhook.Endpoint {
	out := make([]webhook.Endpoint, 0, len(c.Integrations.Webhooks))
	for _, w := range c.Integrations.Webhooks {
		var timeout time.Duration
		if w.Timeout != "" {
			timeout, _ = time.ParseDuration(w.Timeout)
		}
		out = append(out, webhook.Endpoint{
			URL: strings.TrimSpace(w.URL), Secret: w.Secret,
			Events: w.Events, Subsystems: w.Subsystems, Timeout: timeout,
		})
	}
	return out
}
