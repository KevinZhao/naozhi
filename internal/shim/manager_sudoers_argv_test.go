package shim

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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

// findRepoRoot walks upward from the test file until it finds go.mod.
// Scoped to the shim package test process — cheap enough and avoids
// hard-coding a relative `../../` that would break if the package is
// ever moved.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller unavailable")
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("go.mod not found walking up from %s", thisFile)
	return ""
}
