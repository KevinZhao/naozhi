// `naozhi config check` (#2536): validate config.yaml without booting the
// server, and show what the spawn pipeline would actually do with it — the
// full config.Load fatal validation, plus each backend's gate decisions
// (SpawnDiags) and, with -effective, the final argv and (masked) env. This is
// the "改完先验一下" entry the 2026-07-19 rollback and the three-month
// --effort strip (#2412) never had.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/envpolicy"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sysession"
)

func runConfig(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, configUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "check":
		os.Exit(configCheck(args[1:], os.Stdout))
	case "migrate":
		os.Exit(configMigrate(args[1:], os.Stdout))
	case "reload":
		os.Exit(configReload(args[1:], os.Stdout))
	default:
		fmt.Fprintln(os.Stderr, configUsage)
		os.Exit(2)
	}
}

const configUsage = "usage: naozhi config check [-config config.yaml] [-effective] [-json]\n" +
	"       naozhi config migrate [-config config.yaml] [-write]\n" +
	"       naozhi config reload [-addr URL] [-token T] [-timeout D] [-json]"

// backendDiag is one gate decision attributed to the backend whose spawn
// inputs produced it ("" for config-load diags with no backend context).
type backendDiag struct {
	Backend string `json:"backend,omitempty"`
	cli.SpawnDiag
}

// effectiveSpawn is one backend's final spawn inputs, as the runtime would
// build them for a session with no agent and the default access profile.
type effectiveSpawn struct {
	Argv []string `json:"argv"`
	Env  []string `json:"env"`
	// Agents holds the argv for each agent that spawns on this backend (all of
	// them unless agents[].backend pins one), whose system_prompt and args are
	// session-scoped and therefore absent from Argv above.
	Agents map[string][]string `json:"agents,omitempty"`
	// Profiles holds the masked env each access profile's overlay produces on
	// top of Env. Overlay values face the same allowlist + guards, so a
	// refused entry shows up as an env-filter diag, not here.
	Profiles map[string][]string `json:"profiles,omitempty"`
}

// effectiveSessionKey is the placeholder the report resolves session-scoped
// values against. `--debug-file` is a per-session path (a key hash), so a
// report for "this config" has to name the session it is describing.
const effectiveSessionKey = "config-check:placeholder-session"

// checkResult is the -json document; the human output prints the same data.
type checkResult struct {
	Fatal     []string                  `json:"fatal"`
	Diags     []backendDiag             `json:"diags"`
	Effective map[string]effectiveSpawn `json:"effective,omitempty"`
}

// configCheck implements the command; separated from runConfig (which owns
// os.Exit) so tests can assert exit codes and output directly.
// Exit codes: 2 = fatal validation failure, 1 = at least one gate would
// drop/ignore configured input, 0 = clean.
func configCheck(args []string, stdout io.Writer) int {
	fs, configPath := newSubFlagSet("config check", "config.yaml")
	effective := fs.Bool("effective", false, "print each backend's final argv and masked env")
	jsonOut := fs.Bool("json", false, "machine-readable output: {fatal[], diags[], effective{}}")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Backend profiles register explicitly (not via init); EnsureDefaults is
	// the same idempotent bootstrap doctor uses, sharing one sync.Once so the
	// two commands cannot double-register in a single test process (#1165).
	backend.EnsureDefaults()

	result := checkResult{Fatal: []string{}, Diags: []backendDiag{}}

	// Collect the diags config.Load itself emits (deprecated fields, denied
	// flags found by validateArgvStrings) instead of re-implementing them.
	restore := cli.ObserveSpawnDiags(func(_ string, d cli.SpawnDiag) {
		result.Diags = append(result.Diags, backendDiag{SpawnDiag: d})
	})
	cfg, err := config.Load(*configPath)
	restore()
	if err != nil {
		result.Fatal = append(result.Fatal, err.Error())
		emitCheckResult(stdout, result, *jsonOut)
		return 2
	}

	// Per-backend gate decisions: the same SpawnDiagsFor the real spawn path
	// runs, against the same SpawnOptions shape initBackendWrappers feeds it.
	if *effective {
		result.Effective = map[string]effectiveSpawn{}
	}
	// The findings startup logs (logConfigValidationDiagnostics), so each
	// Validate rule reaches this report without a copy here.
	for _, d := range cfg.Validate() {
		reason := d.Msg
		if d.Hint != "" {
			reason += " (" + d.Hint + ")"
		}
		result.Diags = append(result.Diags, backendDiag{SpawnDiag: cli.SpawnDiag{
			Layer: "config-validate", Key: d.Field, Action: d.Level, Reason: reason,
		}})
	}
	result.Diags = append(result.Diags, sysessionBackendDiags(cfg)...)

	// The shim env gate is a spawn gate like the argv one: a var the operator
	// exported that will not reach the CLI belongs in this report, not only in
	// the server's log at spawn time.
	environ := os.Environ()
	filteredEnv := envpolicy.FilterShimEnv(environ)
	for _, d := range envpolicy.ShimEnvDrops(environ) {
		result.Diags = append(result.Diags, backendDiag{SpawnDiag: cli.SpawnDiag{
			Layer: "env-filter", Key: d.Key, Action: "dropped", Reason: d.Reason,
		}})
	}
	// The three argv-bearing paths the runtime resolves at startup. Reporting
	// them means `--effective` shows the --settings / --mcp-config / --debug-file
	// the real spawn passes; omitting them (as this did) described an argv that
	// never happens.
	storePath := osutil.ExpandHome(cfg.Session.StorePath)
	// Path only: the startup path bootstraps the file, and one a check created
	// unseeded would stop the next start from seeding it from local settings.
	settingsFile := ""
	if cfg.NaozhiSettings.Enabled {
		if p, err := naozhiSettingsPath(cfg, storePath); err == nil {
			settingsFile = p
		}
	}
	mcpConfigFile := resolveMCPConfigFile(cfg)
	// Same two-step main() takes: the debug root hangs off the event-log dir,
	// and the file name is the session key hash. CLIDebugDir is the read-only
	// half, so a check never creates a debug directory.
	debugFile := session.CLIDebugPath(
		session.CLIDebugDir(datadir.ForStore(storePath).EventsRoot()), effectiveSessionKey)

	// The same session-layer view NewRouter receives, so default_model resolves
	// through the same lookup the spawn path uses.
	accessProfiles := buildAccessProfiles(cfg.AccessProfiles)

	usable := 0
	for _, b := range cfg.EnabledBackends() {
		id := b.ID
		profile, ok := backend.Get(id)
		if !ok && id == "" {
			id = "claude"
			profile, ok = backend.Get(id)
		}
		if !ok {
			continue // reported by Validate above; the startup path skips it
		}
		usable++
		proto := profile.NewProtocol(backend.ProtocolDeps{})
		caps := cli.ProtocolCaps(proto)
		// The startup path drops a tier the backend cannot accept
		// (initBackendWrappers), so reporting b.Effort verbatim would print an
		// --effort the real spawn never passes. The drop is already reported as
		// a caps diag by SpawnDiagsFor below.
		effort := b.Effort
		if !caps.EffortTier {
			effort = ""
		}
		// The argv chain is the spawn path's own (session.EffectiveArgvLayers →
		// mergeArgvLayers): backend defaults < default access profile's
		// default_model < agent. A hand-written copy here reported agent args as
		// replacing the backend's and ignored default_model (#2969).
		bd := session.BackendDefaults{Model: b.Model, Args: b.Args, Effort: effort}
		baseModel, baseEffort, baseArgs, _ := session.EffectiveArgvLayers(bd, accessProfiles, cfg.DefaultAccessProfile, session.AgentOpts{})
		opts := session.ArgvSpawnOptions(baseModel, baseEffort, debugFile, "", baseArgs, settingsFile, mcpConfigFile)
		for _, d := range cli.SpawnDiagsFor(
			session.ArgvSpawnOptions(b.Model, b.Effort, debugFile, "", b.Args, settingsFile, mcpConfigFile),
			caps,
		) {
			result.Diags = append(result.Diags, backendDiag{Backend: id, SpawnDiag: d})
		}
		if *effective {
			eff := effectiveSpawn{
				Argv: proto.BuildArgs(opts),
				Env:  maskEnvValues(filteredEnv),
			}
			for agentID, ac := range cfg.Agents {
				// An agent's own access_profile outranks default_access_profile at
				// spawn, so its default_model and default_backend are the ones used.
				profileID := cfg.DefaultAccessProfile
				if ac.AccessProfile != "" {
					profileID = ac.AccessProfile
				}
				// An agent whose backend or profile names one spawns there unless
				// the dashboard picks another, so it is listed only under that one.
				if pin := session.EffectiveDefaultBackend(ac.Backend, accessProfiles, profileID); pin != "" && pin != id {
					continue
				}
				model, agentEffort, args, prompt := session.EffectiveArgvLayers(bd, accessProfiles, profileID, session.AgentOpts{
					Model: ac.Model, Effort: ac.Effort, ExtraArgs: ac.Args, SystemPrompt: ac.SystemPrompt,
				})
				// The startup path drops the tier for a backend without one; an
				// agent's own effort meets the same gate at spawn.
				if !caps.EffortTier {
					agentEffort = ""
				}
				if eff.Agents == nil {
					eff.Agents = map[string][]string{}
				}
				eff.Agents[agentID] = proto.BuildArgs(session.ArgvSpawnOptions(
					model, agentEffort, debugFile, prompt, args, settingsFile, mcpConfigFile))
			}
			for apID, ap := range cfg.AccessProfiles {
				if len(ap.Env) == 0 {
					continue
				}
				if eff.Profiles == nil {
					eff.Profiles = map[string][]string{}
				}
				env, diags := effectiveProfileEnv(filteredEnv, apID, ap.Env)
				eff.Profiles[apID] = env
				for _, d := range diags {
					result.Diags = append(result.Diags, backendDiag{Backend: id, SpawnDiag: d})
				}
			}
			result.Effective[id] = eff
		}
	}
	// initBackendWrappers binds no wrapper then, and main exits.
	if usable == 0 {
		result.Fatal = append(result.Fatal, "no usable cli backend configured: "+
			"neither cli.backend nor any cli.backends entry is a registered backend id")
	}
	result.Diags = dedupBackendDiags(result.Diags)

	emitCheckResult(stdout, result, *jsonOut)
	if len(result.Fatal) > 0 {
		return 2
	}
	if len(result.Diags) > 0 {
		return 1
	}
	return 0
}

// dedupBackendDiags drops exact repeats: config.Load's validateArgvStrings
// and the per-backend SpawnDiagsFor both report a denied backend arg.
func dedupBackendDiags(in []backendDiag) []backendDiag {
	seen := map[string]bool{}
	out := in[:0]
	for _, d := range in {
		k := d.Backend + "\x00" + d.Layer + "\x00" + d.Key + "\x00" + d.Action
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, d)
	}
	return out
}

// sensitiveEnvKey reports whether an env key's value must be masked in
// -effective output.
func sensitiveEnvKey(key string) bool {
	up := strings.ToUpper(key)
	for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "API_KEY", "ACCESS_KEY", "AUTH"} {
		if strings.Contains(up, marker) {
			return true
		}
	}
	return false
}

// maskEnvValues masks the value of every sensitive KEY=value entry: first 4
// bytes plus the length, never the full secret.
func maskEnvValues(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i < 0 || !sensitiveEnvKey(kv[:i]) {
			out = append(out, kv)
			continue
		}
		v := kv[i+1:]
		head := v
		if len(head) > 4 {
			head = head[:4]
		}
		out = append(out, fmt.Sprintf("%s=%s…(len=%d)", kv[:i], head, len(v)))
	}
	return out
}

// emitCheckResult prints result as JSON or for humans.
func emitCheckResult(w io.Writer, r checkResult, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(r)
		return
	}
	for _, f := range r.Fatal {
		fmt.Fprintf(w, "FATAL: %s\n", f)
	}
	for _, d := range r.Diags {
		prefix := ""
		if d.Backend != "" {
			prefix = "backend " + d.Backend + ": "
		}
		fmt.Fprintf(w, "DIAG: %s%s %s %s — %s\n", prefix, d.Layer, d.Key, d.Action, d.Reason)
	}
	switch {
	case len(r.Fatal) > 0:
		fmt.Fprintln(w, "config check: FATAL — naozhi would refuse to start")
	case len(r.Diags) > 0:
		// The distinction matters: none of these stop a boot. At runtime the
		// gates warn once and drop the value, which is how a stripped --effort
		// survived three months (#2412) — the exit code and this line are the
		// only things that make it loud. Validate findings drop nothing, so
		// they are counted apart.
		findings := 0
		for _, d := range r.Diags {
			if d.Layer == "config-validate" {
				findings++
			}
		}
		dropped := len(r.Diags) - findings
		if dropped > 0 {
			fmt.Fprintf(w, "config check: %d configured input(s) would not take effect "+
				"(naozhi would still start; each gate warns once at runtime and drops the value)\n", dropped)
		}
		if findings > 0 {
			fmt.Fprintf(w, "config check: %d config warning(s) "+
				"(naozhi would still start; startup logs each one and runs as configured)\n", findings)
		}
	default:
		fmt.Fprintln(w, "config check: OK")
	}
	for id, eff := range r.Effective {
		fmt.Fprintf(w, "\nbackend %s argv:\n", id)
		for _, a := range eff.Argv {
			fmt.Fprintf(w, "  %s\n", a)
		}
		for _, agentID := range sortedKeys(eff.Agents) {
			fmt.Fprintf(w, "backend %s argv for agent %s:\n", id, agentID)
			for _, a := range eff.Agents[agentID] {
				fmt.Fprintf(w, "  %s\n", a)
			}
		}
		fmt.Fprintf(w, "backend %s env (shim-filtered, masked):\n", id)
		for _, kv := range eff.Env {
			fmt.Fprintf(w, "  %s\n", kv)
		}
		for _, apID := range sortedKeys(eff.Profiles) {
			fmt.Fprintf(w, "backend %s env with access profile %s (masked):\n", id, apID)
			for _, kv := range eff.Profiles[apID] {
				fmt.Fprintf(w, "  %s\n", kv)
			}
		}
	}
}

// sortedKeys keeps the human output stable across runs; map iteration order
// would otherwise reshuffle the agent and profile blocks on every invocation.
func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sysessionBackendDiags reports a default backend the sysession runner cannot
// use. Both the daemon framework and image auto-orient shell out with Claude's
// one-shot argv (`-p --output-format json --setting-sources ""`), so a non-claude
// default leaves them spawning a binary that cannot parse it — kiro speaks ACP
// and rejects that argv outright. The backend judged is the one startup binds
// (Config.StartupDefaultBackendID), not cli.backend: with `backends: [kiro, claude]`
// and no cli.backend, serve hands both features kiro.
//
// Static: it reads only cfg, so `naozhi config check` catches it without
// starting a server. sysession.NewRunner refuses the same combination too.
func sysessionBackendDiags(cfg *config.Config) []backendDiag {
	id := cfg.StartupDefaultBackendID()
	if id == sysession.BackendClaude {
		return nil
	}
	why := ""
	if want := cfg.DefaultBackendID(); want != id {
		why = fmt.Sprintf(" (default %q is not an enabled, registered cli.backends entry, so startup falls back to %q)", want, id)
	}
	var out []backendDiag
	add := func(key, what string) {
		out = append(out, backendDiag{Backend: id, SpawnDiag: cli.SpawnDiag{
			Layer: "caps", Key: key, Action: "ignored",
			Reason: fmt.Sprintf("%s needs the %q backend's one-shot argv; %q cannot parse it, so %s never runs%s",
				what, sysession.BackendClaude, id, what, why),
		}})
	}
	if cfg.Sysession.Enabled {
		add("sysession.enabled", "the sysession daemon framework")
	}
	if cfg.ImageOrientEnabled() {
		add("image_orient.enabled", "image auto-orient")
	}
	return out
}

// effectiveProfileEnv is the masked env an access profile's overlay produces
// on top of the filtered baseline, resolved the way the spawn path resolves it
// (session.resolveEnvOverlay + envpolicy.MergeShimEnv). Handing MergeShimEnv the
// raw overlay dropped every *_FILE key silently — it is overlay-allowed but not
// shim-allowed — so the report showed a profile injecting no credential at all
// while the runtime reads the file and injects the concrete key (#2969).
//
// The file is not read here: the entry names its source instead, and an
// unreadable file becomes a diag, because at spawn it is a FAIL-LOUD error.
// Every drop the merge gate makes is a diag too, keyed by profile.
func effectiveProfileEnv(baseline []string, profileID string, overlay map[string]string) ([]string, []cli.SpawnDiag) {
	var diags []cli.SpawnDiag
	resolved := make(map[string]string, len(overlay))
	fromFile := map[string]string{} // concrete key -> file path
	for k, v := range overlay {
		concrete, ok := envpolicy.ResolvedFileKey(k)
		if !ok {
			resolved[k] = v
			continue
		}
		if _, err := os.Stat(v); err != nil {
			diags = append(diags, cli.SpawnDiag{
				Layer: "access-profile", Key: "access_profiles." + profileID + ".env." + k, Action: "ignored",
				Reason: fmt.Sprintf("secret file is not readable (%v); a spawn under this profile fails instead of falling back to the global default", err),
			})
			continue
		}
		fromFile[concrete] = v
		resolved[concrete] = "(from " + v + ")"
	}
	for _, d := range envpolicy.MergeShimEnvDrops(baseline, resolved) {
		diags = append(diags, cli.SpawnDiag{
			Layer: "env-filter", Key: "access_profiles." + profileID + ".env." + d.Key, Action: "dropped", Reason: d.Reason,
		})
	}
	env := maskEnvValues(envpolicy.MergeShimEnv(baseline, resolved))
	for i, kv := range env {
		key := kv
		if j := strings.IndexByte(kv, '='); j >= 0 {
			key = kv[:j]
		}
		if path, ok := fromFile[key]; ok {
			env[i] = key + "=<read from " + path + " at spawn>"
		}
	}
	return env, diags
}
