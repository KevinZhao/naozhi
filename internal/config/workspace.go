package config

import (
	"log/slog"
	"os"
	"strings"
)

// WorkspaceConfig identifies this naozhi instance.
type WorkspaceConfig struct {
	ID   string `yaml:"id"`   // unique identifier (default: hostname)
	Name string `yaml:"name"` // display name (default: id)
}

type ProjectsConfig struct {
	Root            string          `yaml:"root"`                       // projects root directory
	PlannerDefaults PlannerDefaults `yaml:"planner_defaults,omitempty"` // global planner defaults
	// IncludeRoot also registers the projects root itself as a project so files
	// directly under root get preview/download buttons. Default false.
	// SECURITY: the root project spans the whole tree (sibling projects
	// included) and the dashboard token is the only barrier — SINGLE-OPERATOR
	// feature. The file endpoints treat it like the __public_tmp__ pseudo-project
	// (UID / denied-name / irregular-type / credential-name gates, audit log).
	IncludeRoot bool `yaml:"include_root,omitempty"`
	// PublicTmp opts the __public_tmp__ pseudo-project in (#646). Default
	// false: that pseudo-project is a plain "project not found".
	//
	// SECURITY: MUST stay false on any shared / multi-operator deployment, or
	// anywhere the dashboard token is shared. Enabled, every authenticated
	// dashboard user can read non-credential files anywhere under /tmp — the
	// credential allowlist and the foreign-private-UID gate block secrets and
	// sockets, not general content. Accesses are audit-logged at Info
	// ("public_tmp file access"). Same single-operator caveat as IncludeRoot
	// above, and the same file-endpoint gates.
	//
	// Three review items (R242-SEC-6, R244-SEC-P3-2, R245-SEC-7) all asked for
	// exactly this: an operator opt-in flag defaulting to false. The
	// ServerOptions field existed; the config key did not, so the feature was
	// off unconditionally. Wired in #2553's follow-up.
	PublicTmp bool `yaml:"public_tmp,omitempty"`
}

type PlannerDefaults struct {
	Model  string `yaml:"model,omitempty"`
	Prompt string `yaml:"prompt,omitempty"`
}

type NodeConfig struct {
	URL         string `yaml:"url"`
	Token       string `yaml:"token"`
	DisplayName string `yaml:"display_name"`
	Insecure    bool   `yaml:"insecure"` // allow plaintext HTTP without authentication
}

// LogValue implements slog.LogValuer so the bearer Token never lands in logs.
func (c NodeConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("url", c.URL),
		slog.String("token", redactSecret(c.Token)),
		slog.String("display_name", c.DisplayName),
		slog.Bool("insecure", c.Insecure),
	)
}

// UpstreamConfig configures this node to connect as a reverse node to a primary.
type UpstreamConfig struct {
	URL         string `yaml:"url"`
	NodeID      string `yaml:"node_id"`
	Token       string `yaml:"token"`
	DisplayName string `yaml:"display_name"`
	Insecure    bool   `yaml:"insecure"`
}

// LogValue implements slog.LogValuer so the bearer Token never lands in logs.
func (c UpstreamConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("url", c.URL),
		slog.String("node_id", c.NodeID),
		slog.String("token", redactSecret(c.Token)),
		slog.String("display_name", c.DisplayName),
		slog.Bool("insecure", c.Insecure),
	)
}

// redactSecret returns a fixed placeholder for non-empty secrets and "" for
// unset ones, so logs distinguish "configured" from "absent" without leaking length.
func redactSecret(s string) string {
	if s == "" {
		return ""
	}
	return "[REDACTED]"
}

// AccessProfile is a NAMED bundle of "how to reach the model": a whitelisted
// env overlay (auth chain / upstream), default backend and default model.
// Orthogonal to backend — claude can run on 1P direct or Bedrock proxy. The env
// values live only in this operator-authored file; project.yaml (which may
// sync from git) carries only the NAME (RFC project-access-profile §2/§6.1).
type AccessProfile struct {
	// DisplayName is the operator-facing label (dashboard chip / picker).
	DisplayName string `yaml:"display_name,omitempty"`
	// ChipColor is a CSS colour for the dashboard chip (e.g. "#d97757").
	ChipColor string `yaml:"chip_color,omitempty"`
	// Env is the whitelisted overlay (envpolicy.ValidateOverlayEntry); *_FILE
	// keys name a host path whose contents become the secret at spawn time.
	// Merged onto the shim baseline and STILL re-filtered by the shim.
	Env map[string]string `yaml:"env,omitempty"`
	// DefaultModel sits below an explicit per-request / PlannerModel choice and
	// above backend.DefaultModel.
	DefaultModel string `yaml:"default_model,omitempty"`
	// DefaultBackend optionally pins a backend; a project's `backend` still wins.
	DefaultBackend string `yaml:"default_backend,omitempty"`
}

// applyWorkspaceDefaults fills THIS INSTANCE's identity (hostname, then
// "local") and lowercases the agent_commands keys — CJK mobile IMEs
// auto-capitalize "/Review". Not to be confused with Config.Workspaces, the
// remote-node map Normalize reconciles. Split out of applyDefaults (#2710 J11).
func applyWorkspaceDefaults(cfg *Config) {
	if cfg.Workspace.ID == "" {
		if h, err := os.Hostname(); err == nil {
			cfg.Workspace.ID = h
		} else {
			cfg.Workspace.ID = "local"
		}
	}
	if cfg.Workspace.Name == "" {
		cfg.Workspace.Name = cfg.Workspace.ID
	}

	// Lowercase agent_commands keys: CJK mobile IMEs auto-capitalize "/Review".
	// Case conflicts keep the last-written value with a warning.
	if len(cfg.AgentCommands) > 0 {
		normalized := make(map[string]string, len(cfg.AgentCommands))
		for cmd, agentID := range cfg.AgentCommands {
			lower := strings.ToLower(cmd)
			if existing, dup := normalized[lower]; dup && existing != agentID {
				slog.Warn("agent_commands key case conflict after normalize",
					"command", lower, "previous_agent", existing, "new_agent", agentID)
			}
			normalized[lower] = agentID
		}
		cfg.AgentCommands = normalized
	}
}
