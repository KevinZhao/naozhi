package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/config"
)

// TestLogConfigValidationDiagnostics_RoutesByLevel verifies that
// logConfigValidationDiagnostics fans validation findings into slog at the
// right severity: "error" → slog.Error, anything else → slog.Warn. CQ1
// (#396) extracts this logic from main() so a future format / level drift
// is visible at unit-test time instead of only during a journalctl review
// after a startup regression.
//
// The test reproduces the production fail-soft posture: an unknown backend
// ID triggers a single ValidationDiag whose Level matches the "error" arm,
// and main() does NOT abort. A new check that wrongly raises Level beyond
// what config.Validate's contract documents would surface here as an
// unexpected level token in the captured journald output.
func TestLogConfigValidationDiagnostics_RoutesByLevel(t *testing.T) {
	t.Parallel()
	// Backend registry must be populated for cfg.Validate to recognise
	// "claude" as known and flag the typo'd entry as unknown. Idempotent
	// across tests.
	backend.EnsureDefaults() // idempotent across parallel tests

	// Capture slog output in JSON so we can assert on level + message.
	var buf bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	cfg := &config.Config{
		CLI: config.CLIConfig{
			Backends: []config.CLIBackendConfig{
				{ID: "claude", Path: "/usr/local/bin/claude"},
				{ID: "definitely-not-a-real-backend-id", Path: "/tmp/x"},
			},
			Backend: "claude",
		},
	}

	diags := cfg.Validate()
	if len(diags) == 0 {
		t.Fatal("config.Validate returned no diags; test fixture must produce at least the unknown-backend finding")
	}

	logConfigValidationDiagnostics(cfg)

	out := buf.String()
	// Every diag must surface, with its field token preserved verbatim so
	// operators can grep their journal for the offending YAML path.
	for _, d := range diags {
		if !strings.Contains(out, d.Field) {
			t.Errorf("slog output missing diag field %q\nout=%s", d.Field, out)
		}
		if !strings.Contains(out, d.Msg) {
			t.Errorf("slog output missing diag msg %q\nout=%s", d.Msg, out)
		}
		// "error" diags must land on slog.Error; other levels on slog.Warn.
		// JSON handler emits "level":"ERROR" / "WARN" so a substring check
		// is sufficient and survives field reordering.
		var wantLevel string
		switch d.Level {
		case "error":
			wantLevel = `"level":"ERROR"`
		default:
			wantLevel = `"level":"WARN"`
		}
		if !strings.Contains(out, wantLevel) {
			t.Errorf("expected %s level for diag %+v but it was missing\nout=%s", wantLevel, d, out)
		}
	}
}

// TestInitBackendWrappers_NoUsableBackend verifies that the helper signals
// "no usable backend" when every configured ID is unknown. Pre-CQ1 main()
// inlined this check; the regression risk after extraction is the helper
// silently returning ok=true with a nil Default, leading the caller to
// dereference Default.BackendID and crash. The test pins the contract:
// ok=false AND Default==nil when nothing usable was found.
func TestInitBackendWrappers_NoUsableBackend(t *testing.T) {
	t.Parallel()
	backend.EnsureDefaults() // idempotent across parallel tests
	cfg := &config.Config{
		CLI: config.CLIConfig{
			Backends: []config.CLIBackendConfig{
				// Both unknown — initBackendWrappers must skip both and
				// ultimately return ok=false.
				{ID: "ghost-backend-1", Path: "/tmp/never"},
				{ID: "ghost-backend-2", Path: "/tmp/never"},
			},
			Backend: "ghost-backend-1",
		},
	}
	bws, ok := initBackendWrappers(context.Background(), cfg, nil, "", func() string { return "" })
	if ok {
		t.Fatalf("expected ok=false when every backend ID is unknown; got ok=true with Default=%v", bws.Default)
	}
	if bws.Default != nil {
		t.Errorf("expected Default=nil on no-usable-backend path; got %+v", bws.Default)
	}
	if len(bws.Wrappers) != 0 {
		t.Errorf("expected empty Wrappers map on no-usable-backend path; got %d entries", len(bws.Wrappers))
	}
}

// TestInitBackendWrappers_DefaultIDPropagated locks the contract that the
// helper's DefaultID matches cfg.DefaultBackendID(). A regression here
// would cause router.Wrappers / router.DefaultBackend to disagree, and
// session keys without an explicit backend would route to a wrapper that
// session-resolution code does not expect.
func TestInitBackendWrappers_DefaultIDPropagated(t *testing.T) {
	t.Parallel()
	backend.EnsureDefaults() // idempotent across parallel tests
	cfg := &config.Config{
		CLI: config.CLIConfig{
			Backends: []config.CLIBackendConfig{
				{ID: "claude", Path: "/usr/local/bin/claude"},
			},
			Backend: "claude",
		},
	}
	bws, _ := initBackendWrappers(context.Background(), cfg, nil, "", func() string { return "" })
	if bws.DefaultID != cfg.DefaultBackendID() {
		t.Fatalf("DefaultID drift: helper=%q cfg.DefaultBackendID=%q",
			bws.DefaultID, cfg.DefaultBackendID())
	}
}
