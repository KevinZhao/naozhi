//go:build linux

package shim

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestBuildBusctlArgs_ShapeMatchesSudoersPolicy pins the exact argv shape
// that moveToShimsCgroup hands to `sudo`. The shipped sudoers template
// (deploy/naozhi-sudoers.example) depends on every literal operand here
// not drifting — if the argv changes, the sudoers Cmnd_Alias must change
// in lockstep or production will silently fall back to
// moveToShimsCgroupDirect / no-survival mode.
//
// See docs/ops/sudoers-hardening.md for the full rationale and the
// exact policy the sudoers file encodes.
func TestBuildBusctlArgs_ShapeMatchesSudoersPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		scope string
		pids  []int
		want  []string
	}{
		{
			name:  "1 pid — shim only, no CLI yet",
			scope: "naozhi-shim-12345.scope",
			pids:  []int{12345},
			want: []string{
				"-n", "busctl", "call",
				"org.freedesktop.systemd1",
				"/org/freedesktop/systemd1",
				"org.freedesktop.systemd1.Manager",
				"StartTransientUnit",
				"ssa(sv)a(sa(sv))",
				"naozhi-shim-12345.scope", "fail", "2",
				"PIDs", "au", "1",
				"12345",
				"KillMode", "s", "none", "0",
			},
		},
		{
			name:  "2 pids — shim + CLI (the normal hot path)",
			scope: "naozhi-shim-67890.scope",
			pids:  []int{67890, 67891},
			want: []string{
				"-n", "busctl", "call",
				"org.freedesktop.systemd1",
				"/org/freedesktop/systemd1",
				"org.freedesktop.systemd1.Manager",
				"StartTransientUnit",
				"ssa(sv)a(sa(sv))",
				"naozhi-shim-67890.scope", "fail", "2",
				"PIDs", "au", "2",
				"67890", "67891",
				"KillMode", "s", "none", "0",
			},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildBusctlArgs(tc.scope, tc.pids)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("buildBusctlArgs shape drift — sudoers policy must be updated in\n"+
					"  deploy/naozhi-sudoers.example\n"+
					"want %q\ngot  %q", tc.want, got)
			}
		})
	}
}

// TestBuildBusctlArgs_RejectsMalformedScopeName asserts that
// buildBusctlArgs rejects any scopeName that fails the
// `^naozhi-shim-[0-9]+\.scope$` character-set check (R236-SEC-11 /
// R239-SEC-7). Today the sole producer feeds in fmt.Sprintf with %d
// on a validated PID, so the assertion is pure defense-in-depth for
// future call paths that might funnel attacker-derived names through.
func TestBuildBusctlArgs_RejectsMalformedScopeName(t *testing.T) {
	t.Parallel()
	bad := []string{
		"",
		"naozhi-shim-123.scope; rm -rf /",
		"naozhi-shim-abc.scope",      // non-digit PID
		"naozhi-shim-123.SCOPE",      // wrong case suffix
		"prefix/naozhi-shim-1.scope", // path injection
		"naozhi-shim-1.scope\n",      // trailing newline
		"naozhi-shim-1.scope ",       // trailing space
		"systemd-shim-123.scope",     // wrong unit prefix
	}
	for _, name := range bad {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := buildBusctlArgs(name, []int{1})
			if got != nil {
				t.Fatalf("buildBusctlArgs(%q) returned %v, want nil — assertion regressed", name, got)
			}
		})
	}
}

// TestBuildBusctlArgs_AcceptsCanonicalScopeName asserts the regex does
// not reject the production-emitted shape so the assertion is opt-in
// safety, not a new failure mode for the existing call path.
func TestBuildBusctlArgs_AcceptsCanonicalScopeName(t *testing.T) {
	t.Parallel()
	good := []string{
		"naozhi-shim-1.scope",
		"naozhi-shim-12345.scope",
		"naozhi-shim-2147483647.scope",
	}
	for _, name := range good {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := buildBusctlArgs(name, []int{1})
			if got == nil {
				t.Fatalf("buildBusctlArgs(%q) returned nil — assertion is too tight", name)
			}
		})
	}
}

// TestCgroupProcsPath_MatchesSudoersPolicy pins the procs file the
// fallback `sudo tee` writes to. The same literal is hard-coded in
// deploy/naozhi-sudoers.example's NAOZHI_TEE_CGROUP Cmnd_Alias — this
// test rejects any drift between the two.
func TestCgroupProcsPath_MatchesSudoersPolicy(t *testing.T) {
	t.Parallel()
	const want = "/sys/fs/cgroup/naozhi-shims/cgroup.procs"
	if cgroupProcsPath != want {
		t.Fatalf("cgroupProcsPath = %q, want %q — update deploy/naozhi-sudoers.example NAOZHI_TEE_CGROUP together", cgroupProcsPath, want)
	}
}

// TestSudoersExampleMirrorsRuntimeLiterals reads the shipped sudoers
// template from deploy/naozhi-sudoers.example and verifies that each
// runtime-emitted argv literal appears in the policy verbatim. Catches
// the common "fix one side, forget the other" failure mode where
// manager.go grows a new argv but the policy file is never updated,
// and vice-versa (the policy silently going stale).
//
// Not an exhaustive sudoers parser — it looks for the individual
// literals that make the policy meaningful. Anything that the policy
// widens via `*` / `[0-9]*` is not checked here.
func TestSudoersExampleMirrorsRuntimeLiterals(t *testing.T) {
	t.Parallel()
	repoRoot := findRepoRoot(t)
	path := filepath.Join(repoRoot, "deploy", "naozhi-sudoers.example")
	data, err := os.ReadFile(path)
	if err != nil {
		// On platforms without the source tree (e.g. a consumer vendoring
		// the shim package) skip rather than fail.
		if os.IsNotExist(err) {
			t.Skipf("skipping: sudoers example not present at %s", path)
		}
		t.Fatalf("read sudoers example: %v", err)
	}
	got := string(data)

	// Literals from buildBusctlArgs that must appear verbatim. Runtime
	// PIDs and scope names are globbed in the policy and intentionally
	// not on this list.
	literals := []string{
		"/usr/bin/busctl",
		"org.freedesktop.systemd1",
		"/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager",
		"StartTransientUnit",
		`"ssa(sv)a(sa(sv))"`, // quoted in sudoers to survive shell parsing
		"KillMode", "s", "none", "0",
		"PIDs", "au",
		// Fallback path literal
		"/usr/bin/tee",
		cgroupProcsPath,
	}
	for _, lit := range literals {
		if !strings.Contains(got, lit) {
			t.Errorf("deploy/naozhi-sudoers.example missing runtime literal %q — "+
				"runtime argv and sudoers policy have drifted", lit)
		}
	}

	// The policy must reference BOTH Cmnd_Aliases so 1-PID and 2-PID
	// busctl calls are both covered (shim-alone and shim+CLI).
	for _, alias := range []string{"NAOZHI_BUSCTL_1", "NAOZHI_BUSCTL_2", "NAOZHI_TEE_CGROUP"} {
		if !strings.Contains(got, alias) {
			t.Errorf("sudoers example must declare %s Cmnd_Alias", alias)
		}
	}
}

// findRepoRoot moved to testhelper_test.go so cross-platform tests
// (setwritedeadline_contract_test.go) can reuse it without dragging in the
// Linux-only build tag of this file.
