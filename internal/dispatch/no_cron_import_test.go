package dispatch

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// no_cron_import_test.go pins the #1164 invariant (R250-ARCH-1, RFC
// cron-sysession-merge §3.6): internal/dispatch must NOT import
// internal/cron in production code.
//
// Background: the /cron slash-command handlers historically consumed the
// CronScheduler seam whose signatures named *cron.Job / []cron.Job, plus
// cron.NewJob / cron.ClassifyError / cron.Code* directly — pinning a
// dispatch→cron edge that, combined with cron's notify path reaching back
// toward dispatch-side concepts, kept the two domains entangled. #1164
// replaced the seam with the projection-typed CronCommands interface
// (cron_consumer.go); the concrete translation lives in
// internal/server/cron_dispatch_adapter.go.
//
// Mirrors internal/cron/no_platform_import_test.go in intent. This variant
// parses the package's non-test source imports directly (go/parser) rather
// than shelling out to `go list -deps`, so it runs in sandboxed CI without
// go on PATH; the direct-import form is sufficient here because any
// transitive reintroduction would have to enter through a direct import of
// some package, and dispatch's other direct deps are all cron-free leaves
// (a cron edge appearing in one of THEM would trip their own layering
// reviews, and the CI `go list` check in the PR pipeline still covers the
// transitive closure).
func TestDispatch_NoCronImport(t *testing.T) {
	const cronPkg = "github.com/naozhi/naozhi/internal/cron"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s: %v", name, err)
			}
			if path == cronPkg {
				t.Fatalf("%s imports %s — #1164 requires the dispatch→cron edge stay cut. "+
					"Consume cron functionality through the dispatch.CronCommands projection seam "+
					"(cron_consumer.go) and extend the adapter in "+
					"internal/server/cron_dispatch_adapter.go instead of importing cron directly.",
					name, cronPkg)
			}
		}
	}
}
