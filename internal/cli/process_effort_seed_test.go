package cli

// process_effort_seed_test.go — the spawn-pinned effort seed.
//
// `--effort <tier>` is a launch-time pin on both claude and kiro, so the tier
// the wrapper put in argv IS the running tier. Seeding p.effort from
// SpawnOptions.Effort gives Snapshot a value for backends that never report
// one (claude); kiro's _kiro.dev/metadata report still overwrites it. Codex
// takes no tier flag, so nothing is seeded there.
// docs/rfc/dashboard-model-effort-control.md §4.1 (effort chip 入口).

import "testing"

func TestProcess_SeedEffort(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		proto Protocol
		seed  string
		want  string
	}{
		{"claude seeds from spawn pin", &ClaudeProtocol{}, "xhigh", "xhigh"},
		{"kiro seeds from spawn pin", &ACPProtocol{BackendID: "kiro"}, "high", "high"},
		{"empty pin seeds nothing", &ClaudeProtocol{}, "", ""},
		{"codex (no EffortTier) seeds nothing", &CodexProtocol{}, "high", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, srv := shimTestPair(tc.proto)
			defer srv.conn.Close()
			p.seedEffort(tc.seed)
			if got := p.Effort(); got != tc.want {
				t.Errorf("Effort() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A backend-reported tier always wins over the seed, regardless of arrival
// order: metadata after seed overwrites (kiro changes tier mid-session);
// seed after metadata (reconnect path: readLoop may already have consumed a
// replayed frame) must not clobber the report.
func TestProcess_SeedEffort_ReportWins(t *testing.T) {
	t.Parallel()
	t.Run("metadata after seed overwrites", func(t *testing.T) {
		t.Parallel()
		p, srv := shimTestPair(&ACPProtocol{BackendID: "kiro"})
		defer srv.conn.Close()
		p.seedEffort("high")
		p.applyMetadata(&EventMetadata{Effort: "max"})
		if got := p.Effort(); got != "max" {
			t.Errorf("Effort() = %q, want max (report overwrites seed)", got)
		}
	})
	t.Run("seed after metadata keeps report", func(t *testing.T) {
		t.Parallel()
		p, srv := shimTestPair(&ACPProtocol{BackendID: "kiro"})
		defer srv.conn.Close()
		p.applyMetadata(&EventMetadata{Effort: "max"})
		p.seedEffort("high")
		if got := p.Effort(); got != "max" {
			t.Errorf("Effort() = %q, want max (seed must not clobber report)", got)
		}
	})
}

// SeedEffortFromArgs is the reconnect-path seed: the tier is recovered from
// the argv the shim recorded at spawn (shim.State.CLIArgs), the same tokens
// BuildArgs emits.
func TestProcess_SeedEffortFromArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"separate token", []string{"--model", "opus", "--effort", "high", "-p"}, "high"},
		{"equals form", []string{"--effort=low"}, "low"},
		{"no flag", []string{"--model", "opus"}, ""},
		{"dangling flag", []string{"--model", "opus", "--effort"}, ""},
		{"empty args", nil, ""},
		{"last occurrence wins like the CLI", []string{"--effort", "low", "--effort", "max"}, "max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, srv := shimTestPair(&ClaudeProtocol{})
			defer srv.conn.Close()
			p.SeedEffortFromArgs(tc.args)
			if got := p.Effort(); got != tc.want {
				t.Errorf("Effort() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Wrapper.Spawn must actually wire the seed (this is untestable end-to-end
// without a real shim, so pin the argv parity instead): the token the
// protocol emits for SpawnOptions.Effort round-trips through effortFromArgs,
// which is what keeps the fresh-spawn and reconnect seeds in agreement.
func TestEffortFromArgs_RoundTripsBuildArgs(t *testing.T) {
	t.Parallel()
	for _, proto := range []Protocol{&ClaudeProtocol{}, &ACPProtocol{BackendID: "kiro"}} {
		args := proto.BuildArgs(SpawnOptions{Effort: "xhigh"})
		if got := effortFromArgs(args); got != "xhigh" {
			t.Errorf("%s: effortFromArgs(BuildArgs) = %q, want xhigh (argv %v)", proto.Name(), got, args)
		}
	}
	if got := effortFromArgs((&CodexProtocol{}).BuildArgs(SpawnOptions{Effort: "xhigh"})); got != "" {
		t.Errorf("codex argv must carry no --effort; got %q", got)
	}
}
