package config

type AgentConfig struct {
	Model string   `yaml:"model"`
	Args  []string `yaml:"args"`
	// Backend pins the default CLI backend ("claude" | "kiro" | …) for this
	// agent's sessions. Empty = router default.
	Backend string `yaml:"backend,omitempty"`
	// AccessProfile names the default access profile for this agent's
	// sessions. Empty = global default.
	AccessProfile string `yaml:"access_profile,omitempty"`
	// Effort overrides the thinking-effort tier for this agent's sessions.
	// Empty = inherit cli.backends[].effort, then cli.effort.
	Effort string `yaml:"effort,omitempty"`
	// SystemPrompt is appended to the CLI system prompt for every session of
	// this agent (`--append-system-prompt`); planner prompts and scratch
	// context stack on top ("\n\n"-separated). Multi-line is fine; CR/NUL/C0/
	// DEL/C1/bidi and a leading '-' are rejected, capped at
	// MaxAgentSystemPromptBytes. Claude backend only. Do NOT put the flag under
	// `args` — it is denylisted there; Load lifts a legacy occurrence (#2493).
	SystemPrompt string `yaml:"system_prompt,omitempty"`
}

// NaozhiSettingsConfig configures the naozhi-owned isolated Claude settings
// file (RFC naozhi-owned-settings-v3). Zero value = disabled.
type NaozhiSettingsConfig struct {
	// Enabled turns on the naozhi-owned settings file (default false).
	Enabled bool `yaml:"enabled,omitempty"`
	// Path overrides the file location; empty = default under the data root.
	Path string `yaml:"path,omitempty"`
}

type CLIConfig struct {
	// Backend names the default backend ("claude" (default) | "kiro"), used
	// when the dashboard does not pick one for a new session.
	Backend string `yaml:"backend"`
	Path    string `yaml:"path"`
	// Backends enumerates every backend to enable; empty = single-backend mode
	// using Backend/Path/Model/Args.
	Backends []CLIBackendConfig `yaml:"backends,omitempty"`
	Model    string             `yaml:"model"`
	Args     []string           `yaml:"args"`
	// MCPConfig is an absolute path to an MCP server definition file passed via
	// `--mcp-config`; empty passes no flag. Needed when NaozhiSettings is
	// enabled (that path suppresses ~/.claude.json mcpServers). Must be an
	// existing JSON file with an `mcpServers` object — cc refuses to start
	// otherwise, so cmd wiring validates and degrades to "no MCP". Inline JSON
	// is not supported. Recommended mode 0600: writers get arbitrary command
	// execution in every session. Deliberately global, not per-backend/agent.
	MCPConfig string `yaml:"mcp_config,omitempty"`
	// Effort is the default thinking-effort tier for backends that accept one
	// (kiro: low/medium/high/xhigh/max). Empty passes no flag so the backend
	// keeps its own default (docs/rfc/kiro-effort-control.md).
	Effort string `yaml:"effort,omitempty"`
}

// CLIBackendConfig configures one backend in a multi-backend deployment.
// ID is required; Path/Model/Args/Effort fall back to the top-level cli.* values.
type CLIBackendConfig struct {
	ID    string   `yaml:"id"`              // "claude" | "kiro"
	Path  string   `yaml:"path,omitempty"`  // overrides cli.path for this backend
	Model string   `yaml:"model,omitempty"` // overrides cli.model for this backend
	Args  []string `yaml:"args,omitempty"`  // overrides cli.args for this backend
	// Effort overrides cli.effort for this backend. On a backend without a
	// tier flag it is warned and dropped at startup, not a hard error, because
	// cli.effort propagates to EVERY backend via EnabledBackends.
	Effort string `yaml:"effort,omitempty"`
	// Models optionally declares the dashboard model-popover list for this
	// backend (mainly claude; kiro's agent-reported list wins). Each entry is
	// validated like `model`.
	Models []string `yaml:"models,omitempty"`
}

// knownBackendIDs returns the set of enabled backend IDs (never empty) for
// referential validation of `backend` fields.
func (c *Config) knownBackendIDs() map[string]bool {
	ids := make(map[string]bool)
	for _, b := range c.EnabledBackends() {
		ids[b.ID] = true
	}
	return ids
}

// EnabledBackends returns the normalized list of backends to enable: cli.backends
// if set, else the single cli.backend (default "claude"). The default backend is
// always at position 0; duplicate IDs collapse to the first occurrence.
func (c *Config) EnabledBackends() []CLIBackendConfig {
	// Must resolve identically to DefaultBackendID so [0].ID agrees with it.
	defaultID := c.CLI.Backend
	if defaultID == "" {
		for _, b := range c.CLI.Backends {
			if b.ID != "" {
				defaultID = b.ID
				break
			}
		}
	}
	if defaultID == "" {
		defaultID = "claude"
	}

	if len(c.CLI.Backends) == 0 {
		return []CLIBackendConfig{{
			ID:     defaultID,
			Path:   c.CLI.Path,
			Model:  c.CLI.Model,
			Args:   c.CLI.Args,
			Effort: c.CLI.Effort,
		}}
	}

	seen := make(map[string]bool, len(c.CLI.Backends))
	out := make([]CLIBackendConfig, 0, len(c.CLI.Backends))
	for _, b := range c.CLI.Backends {
		if b.ID == "" || seen[b.ID] {
			continue
		}
		seen[b.ID] = true
		if b.Model == "" {
			b.Model = c.CLI.Model
		}
		if len(b.Args) == 0 {
			b.Args = c.CLI.Args
		}
		if b.Effort == "" {
			b.Effort = c.CLI.Effort
		}
		out = append(out, b)
	}

	// All entries had empty IDs: fall back to single-backend mode.
	if len(out) == 0 {
		return []CLIBackendConfig{{
			ID:     defaultID,
			Path:   c.CLI.Path,
			Model:  c.CLI.Model,
			Args:   c.CLI.Args,
			Effort: c.CLI.Effort,
		}}
	}

	// Default backend floats to position 0 regardless of YAML order.
	for i, b := range out {
		if b.ID == defaultID && i > 0 {
			out[0], out[i] = out[i], out[0]
			break
		}
	}
	return out
}

// DefaultBackendID reports the backend ID to use when a request does not
// specify one.
func (c *Config) DefaultBackendID() string {
	if id := c.CLI.Backend; id != "" {
		return id
	}
	if len(c.CLI.Backends) > 0 && c.CLI.Backends[0].ID != "" {
		return c.CLI.Backends[0].ID
	}
	return "claude"
}
