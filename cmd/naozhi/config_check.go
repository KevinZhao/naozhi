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
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/envpolicy"
)

func runConfig(args []string) {
	if len(args) == 0 || args[0] != "check" {
		fmt.Fprintln(os.Stderr, "usage: naozhi config check [-config config.yaml] [-effective] [-json]")
		os.Exit(2)
	}
	os.Exit(configCheck(args[1:], os.Stdout))
}

// backendDiag is one gate decision attributed to the backend whose spawn
// inputs produced it ("" for config-load diags with no backend context).
type backendDiag struct {
	Backend string `json:"backend,omitempty"`
	cli.SpawnDiag
}

// effectiveSpawn is one backend's final spawn inputs.
type effectiveSpawn struct {
	Argv []string `json:"argv"`
	Env  []string `json:"env"`
}

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
	filteredEnv := envpolicy.FilterShimEnv(os.Environ())
	for _, b := range cfg.EnabledBackends() {
		id := b.ID
		profile, ok := backend.Get(id)
		if !ok && id == "" {
			id = "claude"
			profile, ok = backend.Get(id)
		}
		if !ok {
			result.Diags = append(result.Diags, backendDiag{Backend: b.ID, SpawnDiag: cli.SpawnDiag{
				Layer: "caps", Key: "cli.backends[" + b.ID + "]", Action: "ignored",
				Reason: "unknown backend id; the startup path skips this entry",
			}})
			continue
		}
		proto := profile.NewProtocol(backend.ProtocolDeps{})
		opts := cli.SpawnOptions{Model: b.Model, Effort: b.Effort, ExtraArgs: b.Args}
		for _, d := range cli.SpawnDiagsFor(opts, cli.ProtocolCaps(proto)) {
			result.Diags = append(result.Diags, backendDiag{Backend: id, SpawnDiag: d})
		}
		if *effective {
			result.Effective[id] = effectiveSpawn{
				Argv: proto.BuildArgs(opts),
				Env:  maskEnvValues(filteredEnv),
			}
		}
	}
	result.Diags = dedupBackendDiags(result.Diags)

	emitCheckResult(stdout, result, *jsonOut)
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
		fmt.Fprintf(w, "config check: %d configured input(s) would not take effect\n", len(r.Diags))
	default:
		fmt.Fprintln(w, "config check: OK")
	}
	for id, eff := range r.Effective {
		fmt.Fprintf(w, "\nbackend %s argv:\n", id)
		for _, a := range eff.Argv {
			fmt.Fprintf(w, "  %s\n", a)
		}
		fmt.Fprintf(w, "backend %s env (shim-filtered, masked):\n", id)
		for _, kv := range eff.Env {
			fmt.Fprintf(w, "  %s\n", kv)
		}
	}
}
