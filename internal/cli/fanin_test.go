package cli

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const modPrefix = "github.com/naozhi/naozhi/"

// cliImporters is every internal/ package outside internal/cli/... whose
// production code imports internal/cli: the ones that drive a CLI. A package
// that only names CLI vocabulary (states, death reasons, backend rows, the
// argv denylist) takes it from internal/cliinfo instead (#2937).
var cliImporters = []string{
	"internal/dashboard/ext/cli",
	"internal/session",
	// session's backend table: its rows hold the *cli.Wrapper each backend
	// spawns through (#2939).
	"internal/session/backendstore",
	"internal/upstream",
	"internal/wireup",
}

// goList runs `go list` with args and returns its output lines.
func goList(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := exec.Command("go", append([]string{"list"}, args...)...).Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// The set of packages that import internal/cli is exactly cliImporters, in
// both directions: a new importer is a dependency on the CLI machinery to
// justify, and one that stopped importing it shrinks the list.
func TestCLIFanIn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping `go list` walk in -short mode")
	}
	var got []string
	for _, line := range goList(t, "-f", "{{.ImportPath}} {{join .Imports \" \"}}", modPrefix+"internal/...") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pkg := strings.TrimPrefix(fields[0], modPrefix)
		if pkg == "internal/cli" || strings.HasPrefix(pkg, "internal/cli/") {
			continue
		}
		if slices.Contains(fields[1:], modPrefix+"internal/cli") {
			got = append(got, pkg)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, cliImporters) {
		t.Errorf("packages importing internal/cli = %v, want %v", got, cliImporters)
	}
}

// Packages that name CLI vocabulary or process images without driving a CLI:
// nothing in their build graph may reach internal/cli, directly or through
// another package.
func TestNoCLIInClosure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping `go list -deps` walk in -short mode")
	}
	for _, pkg := range []string{"internal/session/sessionview", "internal/imageorient", "internal/cliinfo"} {
		for _, dep := range goList(t, "-deps", modPrefix+pkg) {
			if dep == modPrefix+"internal/cli" {
				t.Errorf("%s transitively imports internal/cli; take CLI vocabulary from internal/cliinfo", pkg)
			}
		}
	}
}

// cliinfo imports only the standard library, so any package can use it.
func TestCLIInfo_StdlibOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping `go list -deps` walk in -short mode")
	}
	for _, dep := range goList(t, "-deps", modPrefix+"internal/cliinfo") {
		if strings.HasPrefix(dep, modPrefix) && dep != modPrefix+"internal/cliinfo" {
			t.Errorf("internal/cliinfo depends on %s; it must import only the standard library", dep)
		}
	}
}
