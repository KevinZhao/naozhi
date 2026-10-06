package turn

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const modulePrefix = "github.com/naozhi/naozhi/"

// TestDependencyBoundary is G-e (#2897 T3004): turn's production dependency
// closure must not reach dispatch, server, platform or session, so both entry
// points can depend on turn and it on neither. Its direct in-module imports
// are pinned too, so a new edge is a reviewed edit here.
func TestDependencyBoundary(t *testing.T) {
	t.Parallel()
	deps := goList(t, "-deps", modulePrefix+"internal/turn")
	if !slices.Contains(deps, modulePrefix+"internal/turn") {
		t.Fatalf("go list -deps output lacks internal/turn itself: %v", deps)
	}
	for _, d := range deps {
		rel, ok := strings.CutPrefix(d, modulePrefix+"internal/")
		if !ok || rel == "session/sessionview" {
			continue
		}
		for _, banned := range []string{"dispatch", "server", "platform", "session"} {
			if rel == banned || strings.HasPrefix(rel, banned+"/") {
				t.Errorf("internal/turn depends on %s; the turn port must not import an entry point or the session package", d)
			}
		}
	}

	var direct []string
	for _, imp := range goList(t, "-f", `{{join .Imports "\n"}}`, modulePrefix+"internal/turn") {
		if rel, ok := strings.CutPrefix(imp, modulePrefix); ok {
			direct = append(direct, rel)
		}
	}
	want := []string{
		"internal/cli/clierr",
		"internal/cli/clievent",
		"internal/ctxutil", // run id / session key on the turn ctx (#3436); leaf
		"internal/limits",
		"internal/metrics",
		"internal/session/sessionview",
		"internal/textutil",
	}
	if !slices.Equal(direct, want) {
		t.Errorf("internal/turn's in-module imports = %v, want %v", direct, want)
	}
}

func goList(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := exec.Command("go", append([]string{"list"}, args...)...).Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	return strings.Fields(string(out))
}
