package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/envpolicy"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/tuningspec"
)

// ValidationDiag is a non-fatal finding from Config.Validate(). A slice of
// diags (not an error) implements the multi-backend RFC §11.1 warn-and-continue
// boot semantics: the caller picks the log level, tests assert on fields.
type ValidationDiag struct {
	// Level is "warn" or "error".
	Level string
	// Field is the dotted YAML path to the offending key, e.g. "cli.backends[gemini]".
	Field string
	// Msg describes what's wrong without repeating the field name.
	Msg string
	// Hint is an optional remediation pointer.
	Hint string
}

// Validate reports non-fatal config mistakes that must not block startup but
// the operator needs to see — currently cli.backends IDs missing from the
// backend.Profile registry. MUST be called after backend.RegisterDefaults()
// or every backend is flagged unknown.
func (c *Config) Validate() []ValidationDiag {
	var diags []ValidationDiag

	backends := c.EnabledBackends()
	for _, b := range backends {
		if b.ID == "" {
			// Single-backend fallback placeholder; main resolves it to claude.
			continue
		}
		if _, ok := backend.Get(b.ID); !ok {
			diags = append(diags, ValidationDiag{
				Level: "error",
				Field: fmt.Sprintf("cli.backends[%s]", b.ID),
				Msg:   "unknown backend id; will be skipped at runtime",
				Hint:  "valid ids: " + strings.Join(knownBackendIDs(), ", "),
			})
		}
	}

	return diags
}

// knownBackendIDs returns every registered backend ID, sorted for
// deterministic hints; a sentinel string when nothing is registered so a
// Validate() before RegisterDefaults is visible in logs as a programmer error.
func knownBackendIDs() []string {
	all := backend.All()
	if len(all) == 0 {
		return []string{"(none registered)"}
	}
	ids := make([]string, len(all))
	for i, p := range all {
		ids[i] = p.ID
	}
	sort.Strings(ids)
	return ids
}

// validateArgvBearingFields gates every configured value that reaches
// exec.Command through BuildArgs (args, model, effort, prompts) plus the access
// profiles: configured argv never passes the user-input validators, so the NUL/C0
// and flag-injection checks happen here. Split out of validateConfig (#2710).
func validateArgvBearingFields(cfg *Config) error {
	// Configured argv never passes the user-input validators, so the
	// NUL/C0 and flag-injection gates are applied here for every field that
	// reaches exec.Command via BuildArgs (args, model, effort, prompts).
	if err := validateArgvStrings("cli.args", cfg.CLI.Args); err != nil {
		return err
	}
	if err := validateModelString("cli.model", cfg.CLI.Model); err != nil {
		return err
	}
	if err := validateEffortString("cli.effort", cfg.CLI.Effort); err != nil {
		return err
	}
	for _, b := range cfg.CLI.Backends {
		if err := validateArgvStrings(fmt.Sprintf("cli.backends[%s].args", b.ID), b.Args); err != nil {
			return err
		}
		if err := validateModelString(fmt.Sprintf("cli.backends[%s].model", b.ID), b.Model); err != nil {
			return err
		}
		if err := validateEffortString(fmt.Sprintf("cli.backends[%s].effort", b.ID), b.Effort); err != nil {
			return err
		}
		for i, m := range b.Models {
			if m == "" {
				return fmt.Errorf("cli.backends[%s].models[%d] is empty", b.ID, i)
			}
			if err := validateModelString(fmt.Sprintf("cli.backends[%s].models[%d]", b.ID, i), m); err != nil {
				return err
			}
		}
	}
	for id, a := range cfg.Agents {
		if err := validateArgvStrings(fmt.Sprintf("agents[%s].args", id), a.Args); err != nil {
			return err
		}
		if err := validateModelString(fmt.Sprintf("agents[%s].model", id), a.Model); err != nil {
			return err
		}
		if err := validateEffortString(fmt.Sprintf("agents[%s].effort", id), a.Effort); err != nil {
			return err
		}
		if err := validateSystemPrompt(fmt.Sprintf("agents[%s].system_prompt", id), a.SystemPrompt); err != nil {
			return err
		}
	}
	if err := validateModelString("projects.planner_defaults.model", cfg.Projects.PlannerDefaults.Model); err != nil {
		return err
	}
	if err := validateModelString("image_orient.model", cfg.ImageOrient.Model); err != nil {
		return err
	}
	if err := validateModelString("sysession.runner.model", cfg.Sysession.Runner.Model); err != nil {
		return err
	}
	// 与 project.PlannerPrompt 同源进 --append-system-prompt argv；LF/CR 同样
	// 禁止，多行 prompt 须经 CLAUDE.md 引入。
	if err := validatePlannerPrompt("projects.planner_defaults.prompt", cfg.Projects.PlannerDefaults.Prompt); err != nil {
		return err
	}

	if err := validateAccessProfiles(cfg); err != nil {
		return err
	}
	return nil
}

// validateAccessProfiles checks env overlays (envpolicy.ValidateOverlayEntry)
// and that every backend / access_profile reference names an enabled or
// defined entry. Unknown names are ERRORS: a typo silently billing a personal
// project to a company account is the mis-charge this feature prevents.
func validateAccessProfiles(cfg *Config) error {
	backends := cfg.knownBackendIDs()
	for name, ap := range cfg.AccessProfiles {
		for k, v := range ap.Env {
			if err := envpolicy.ValidateOverlayEntry(k, v); err != nil {
				return fmt.Errorf("access_profiles[%s].env: %w", name, err)
			}
		}
		if ap.DefaultBackend != "" && !backends[ap.DefaultBackend] {
			return fmt.Errorf("access_profiles[%s].default_backend %q is not an enabled backend", name, ap.DefaultBackend)
		}
		if err := validateModelString(fmt.Sprintf("access_profiles[%s].default_model", name), ap.DefaultModel); err != nil {
			return err
		}
	}
	for id, a := range cfg.Agents {
		if a.Backend != "" && !backends[a.Backend] {
			return fmt.Errorf("agents[%s].backend %q is not an enabled backend", id, a.Backend)
		}
		if a.AccessProfile != "" {
			if _, ok := cfg.AccessProfiles[a.AccessProfile]; !ok {
				return fmt.Errorf("agents[%s].access_profile %q is not defined in access_profiles", id, a.AccessProfile)
			}
		}
	}
	if cfg.DefaultAccessProfile != "" {
		if _, ok := cfg.AccessProfiles[cfg.DefaultAccessProfile]; !ok {
			return fmt.Errorf("default_access_profile %q is not defined in access_profiles", cfg.DefaultAccessProfile)
		}
	}
	return nil
}

// validatePlannerPrompt mirrors project.ValidateConfig's PlannerPrompt policy:
// reject NUL, all C0 (incl. LF/CR), DEL, C1, bidi and LS/PS; empty is allowed.
// Shares project.MaxPlannerPromptBytes so the caps cannot drift.
func validatePlannerPrompt(field, prompt string) error {
	if prompt == "" {
		return nil
	}
	if len(prompt) > project.MaxPlannerPromptBytes {
		return fmt.Errorf("%s exceeds %d-byte limit", field, project.MaxPlannerPromptBytes)
	}
	for i := 0; i < len(prompt); i++ {
		c := prompt[i]
		if c == 0 || (c < 0x20 && c != '\t') || c == 0x7f {
			return fmt.Errorf("%s contains invalid control characters (NUL/C0/DEL — argv corruption guard)", field)
		}
	}
	for _, r := range prompt {
		if osutil.IsLogInjectionRune(r) {
			return fmt.Errorf("%s contains invalid unicode controls (C1/bidi/LS-PS)", field)
		}
	}
	return nil
}

// validateArgvStrings rejects empty elements (YAML "- " typo), NUL and control
// bytes in argv. It WARNS (not errors) on flags cli.BuildArgs would strip as
// denied, naming the field so the operator can move the value to its dedicated
// config key (#2493); `--append-system-prompt` under agents[].args is lifted
// by liftLegacySystemPromptArgs before this runs.
func validateArgvStrings(field string, args []string) error {
	for i, a := range args {
		if a == "" {
			return fmt.Errorf("%s[%d] is empty — refusing (likely YAML typo)", field, i)
		}
		for _, r := range a {
			if r == 0 || (r < 0x20 && r != '\t') || r == 0x7f {
				return fmt.Errorf("%s[%d] contains control byte (0x%02x) — refusing (argv injection guard)", field, i, r)
			}
		}
		if cli.IsDeniedExtraFlag(a) {
			cli.EmitSpawnDiags("config", []cli.SpawnDiag{{
				Layer: "argv-denylist", Key: a, Action: "dropped",
				Reason: fmt.Sprintf("%s[%d]: the spawn pipeline strips this flag; it will NOT reach the CLI — use the dedicated config field instead", field, i),
			}})
		}
	}
	return nil
}

// validateEffortString gates a configured thinking-effort tier; empty means
// "pass no flag".
func validateEffortString(field, value string) error {
	return tuningspec.ValidateEffort(field, value)
}

// validateModelString gates a configured model identifier; empty is allowed
// (caller-defined fallback).
func validateModelString(field, value string) error {
	return tuningspec.ValidateModel(field, value)
}
