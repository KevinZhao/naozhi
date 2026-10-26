package config

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level naozhi configuration loaded from config.yaml.
//
// Two distinct concepts share the word "workspace" and are NOT
// interchangeable: Config.Workspace (this instance's identity) and
// Config.Workspaces (the remote-nodes map).
type Config struct {
	// Fingerprint is the raw-bytes identity of the loaded file (#2538).
	// Populated by Load; zero for programmatically constructed configs.
	Fingerprint Fingerprint `yaml:"-"`

	// SchemaVersion pins the config schema this file targets. Absent/0 is
	// recorded as CurrentSchemaVersion; newer than the binary is rejected.
	// There is no migration: v1 keys only draw a config-deprecated WARN.
	SchemaVersion int `yaml:"schema_version,omitempty"`

	Server        ServerConfig           `yaml:"server"`
	CLI           CLIConfig              `yaml:"cli"`
	Session       SessionConfig          `yaml:"session"`
	Platforms     PlatformConfigs        `yaml:"platforms"`
	Agents        map[string]AgentConfig `yaml:"agents"`
	AgentCommands map[string]string      `yaml:"agent_commands"`
	// AccessProfiles are named auth/upstream overlays a project or agent may
	// reference by name; empty keeps every session on the global settings.json
	// baseline (RFC project-access-profile).
	AccessProfiles map[string]AccessProfile `yaml:"access_profiles,omitempty"`
	// DefaultAccessProfile applies to any session with NO explicit profile
	// (per-request, dashboard, project or resume-locked). Empty = global
	// baseline. Must name a key in AccessProfiles (validated at load).
	DefaultAccessProfile string `yaml:"default_access_profile,omitempty"`
	// NaozhiSettings opts in to a naozhi-owned isolated Claude settings file
	// (seeded once from ~/.claude/settings.json, then decoupled); disabled keeps
	// `--setting-sources user` (RFC naozhi-owned-settings-v3).
	NaozhiSettings NaozhiSettingsConfig `yaml:"naozhi_settings,omitempty"`

	// Workspaces is the remote-instance map (the v1 `nodes` key is no longer
	// read).
	Workspaces   map[string]NodeConfig       `yaml:"workspaces"`
	ReverseNodes map[string]ReverseNodeEntry `yaml:"reverse_nodes"`
	Upstream     *UpstreamConfig             `yaml:"upstream"`
	// Workspace identifies THIS naozhi instance (not Workspaces).
	Workspace   WorkspaceConfig   `yaml:"workspace"`
	Transcribe  *TranscribeConfig `yaml:"transcribe"`
	Cron        CronConfig        `yaml:"cron"`
	Log         LogConfig         `yaml:"log"`
	Projects    ProjectsConfig    `yaml:"projects"`
	Sysession   SysessionConfig   `yaml:"sysession,omitempty"`
	Update      UpdateConfig      `yaml:"update,omitempty"`
	ImageOrient ImageOrientConfig `yaml:"image_orient,omitempty"`
	Cost        CostConfig        `yaml:"cost,omitempty"`
	// IMAccess restricts which IM senders the dispatcher serves (im_access.go).
	IMAccess IMAccessConfig `yaml:"im_access,omitempty"`
	// Integrations are outbound hooks (integrations.go).
	Integrations IntegrationsConfig `yaml:"integrations,omitempty"`
	// IMRateLimit caps each IM sender's message rate (im_rate_limit.go).
	IMRateLimit IMRateLimitConfig `yaml:"im_rate_limit,omitempty"`

	// Parsed durations, populated once in Load.
	cachedTTL             time.Duration `yaml:"-"`
	cachedPruneTTL        time.Duration `yaml:"-"`
	cachedNoOutputTimeout time.Duration `yaml:"-"`
	cachedTotalTimeout    time.Duration `yaml:"-"`
	cachedExecTimeout     time.Duration `yaml:"-"`
	cachedCollectDelay    time.Duration `yaml:"-"`
	cachedJitterMax       time.Duration `yaml:"-"`
	cachedInterval        time.Duration `yaml:"-"`

	// Values whose consumers fall back to a default, populated once in Load by
	// resolveFallbackValues; zero means unset or unusable.
	cachedShimIdleTimeout time.Duration      `yaml:"-"`
	cachedSysession       sysessionDurations `yaml:"-"`
}

// Fingerprint identifies WHICH config file bytes a process loaded: sha256 of
// the raw file (before env expansion, so a placeholder edit changes it and
// default-filling cannot mask an edit), plus when and from where. /health
// exposes it (auth-only) so doctor and the deploy playbook can tell "disk
// config changed after the process loaded it — restart required" (#2538).
type Fingerprint struct {
	SHA256   string
	LoadedAt time.Time
	Path     string
}

// Load reads and parses a YAML config file.
func Load(path string) (*Config, error) {
	// The file carries secrets: reject symlinks (Lstat, so a link to a 0644
	// file cannot bypass the mode gate) and any group/world bit BEFORE reading;
	// the fd re-check below closes the Lstat→open TOCTOU window.
	if fi, statErr := os.Lstat(path); statErr == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("config file %s is a symlink; refusing to load (resolve the link or point --config at the target directly)",
				path)
		}
		if fi.Mode()&0o077 != 0 {
			return nil, fmt.Errorf("config file %s is group/world-accessible (mode %04o); restrict with: chmod 0600 %s",
				path, fi.Mode().Perm(), path)
		}
	}

	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	defer f.Close()
	// Re-check on the open fd with the SAME 0o077 mask so a symlink swap in
	// the Lstat→OpenFile gap cannot load a permissive target. fd-stat failure
	// is fatal: skipping the gates would let an attacker who can interrupt
	// Fstat bypass the second check.
	fi, ferr := f.Stat()
	if ferr != nil {
		return nil, fmt.Errorf("stat config fd: %w", ferr)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("config file %s is not a regular file", path)
	}
	if fi.Mode()&0o077 != 0 {
		return nil, fmt.Errorf("config file %s is group/world-accessible (mode %04o); restrict with: chmod 0600 %s",
			path, fi.Mode().Perm(), path)
	}
	// 1 MiB cap: a runaway or hostile file must not be read whole into memory.
	const maxConfigBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("config file %s exceeds %d bytes", path, maxConfigBytes)
	}

	fp := Fingerprint{
		SHA256:   fmt.Sprintf("%x", sha256.Sum256(data)),
		LoadedAt: time.Now(),
		Path:     path,
	}

	expanded := expandEnvVars(data)

	var doc yaml.Node
	if err := yaml.Unmarshal(expanded, &doc); err != nil {
		// yaml.v3 echoes the offending line, which after ${VAR} expansion may
		// contain secrets; keep the detail in logs only.
		slog.Debug("config yaml parse failed", "err", err)
		return nil, fmt.Errorf("parse config: yaml syntax error (check naozhi logs for details)")
	}
	var cfg Config
	if err := doc.Decode(&cfg); err != nil {
		// The file parsed, so this is a type or duplicate-key error, not a
		// syntax one; describeDecodeError keeps its line numbers and drops the
		// echoed values for the same secrecy reason as above.
		slog.Debug("config yaml decode failed", "err", err)
		return nil, fmt.Errorf("parse config: %s (see naozhi debug logs for details)", describeDecodeError(err))
	}
	// The decode above ignores unknown keys, so report them before defaults are
	// applied — a misspelled key is operator input that had no effect, same as a
	// deprecated field or a denied flag (unknown_keys.go, #2639). The schema v1
	// spellings get their own config-deprecated diag naming the replacement.
	// Diagnostic only: cfg is already fully decoded and is not touched here.
	reportUnknownKeys(expanded, reportDeprecatedKeys(&doc))

	applyDefaults(&cfg)
	if err := parseDurations(&cfg); err != nil {
		return nil, err
	}
	resolveFallbackValues(&cfg)
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}

	cfg.Fingerprint = fp
	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = CurrentSchemaVersion
	}
	// Section by section, in the order the single function ran them.
	applyServerDefaults(cfg)
	applySessionDefaults(cfg)
	applyUpdateDefaults(cfg)
	applyWorkspaceDefaults(cfg)
}

func validateConfig(cfg *Config) error {
	// A newer schema would be silently mis-parsed (unknown keys dropped);
	// fail loud instead.
	if cfg.SchemaVersion > CurrentSchemaVersion {
		return fmt.Errorf("config schema_version %d is newer than this binary supports (max %d); upgrade naozhi or lower schema_version",
			cfg.SchemaVersion, CurrentSchemaVersion)
	}
	// One call per section, in the order the single 240-line function ran them:
	// each returns on its first problem, so the order decides which of two bad
	// values the operator is told about.
	for _, check := range []func(*Config) error{
		validatePlatforms,
		validateNodes,
		validateServer,
		validateNotifyTargets,
		validateArgvBearingFields,
		validateAgentCommands,
		validateProjects,
		validateIMAccess,
		validateIntegrations,
		validateIMRateLimit,
		validateCostBudget,
		validateGroupScope,
	} {
		if err := check(cfg); err != nil {
			return err
		}
	}
	return nil
}
