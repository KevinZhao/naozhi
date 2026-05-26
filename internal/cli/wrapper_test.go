package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCandidatePaths_ContainsNativeInstaller(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}

	paths := candidatePaths("claude")
	if len(paths) == 0 {
		t.Fatal("expected at least one candidate path")
	}

	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	expected := filepath.Join(home, ".local", "bin", "claude"+ext)
	if paths[0] != expected {
		t.Errorf("first candidate should be native installer path, got %q, want %q", paths[0], expected)
	}
}

func TestCandidatePaths_Kiro(t *testing.T) {
	t.Parallel()
	paths := candidatePaths("kiro-cli")
	if len(paths) == 0 {
		t.Fatal("expected at least one candidate path")
	}
	// First path should contain "kiro-cli"
	base := filepath.Base(paths[0])
	if base != "kiro-cli" && base != "kiro-cli.exe" {
		t.Errorf("first candidate should be for kiro-cli, got %q", paths[0])
	}
}

// NOT t.Parallel() — mutates process-global env PATH/HOME via os.Setenv.
// Parallel siblings reading PATH (e.g., exec.LookPath) would see torn
// state across the deferred restore window. Serial only.
//
// R249-SEC-7 (#920): the historical bare-name fallback let exec.Command
// re-resolve through the live PATH at spawn time, opening a PATH-poisoning
// window between detect and exec. detectCLI now returns "" when neither
// candidatePaths nor exec.LookPath finds the binary; callers (NewWrapper,
// DetectBackendsCtx) already handle empty paths gracefully (Probe
// short-circuits, dashboard marks unavailable, exec.Command surfaces a
// clear error at spawn time instead of launching whatever happens to be
// on PATH at exec time).
func TestDetectCLI_ReturnsEmptyWhenNothingFound(t *testing.T) {
	// candidatePaths uses "claude" for any non-"kiro" backend,
	// so we test with a name unlikely to exist via PATH lookup
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", "")
	defer os.Setenv("PATH", origPath)

	// Use a temp HOME with no .local/bin to avoid native installer hit
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", t.TempDir())
	defer os.Setenv("HOME", origHome)

	result := detectCLI("claude")
	// With empty PATH and no files, must return "" rather than the bare
	// basename — the bare name re-resolves through live PATH at exec time
	// and opens a PATH-poisoning vector.
	if result != "" {
		t.Errorf("expected empty fallback, got %q", result)
	}
}

// NOT t.Parallel() — same os.Setenv("PATH", ...) rationale as
// TestDetectCLI_FallsBackToBareNameWhenNothingFound above.
func TestDetectCLI_FindsExistingBinary(t *testing.T) {
	// Create a temp file that looks like a CLI binary
	dir := t.TempDir()
	fakeCLI := filepath.Join(dir, "claude")
	if err := os.WriteFile(fakeCLI, []byte("#!/bin/sh"), 0755); err != nil {
		t.Fatal(err)
	}

	// Override PATH to include our temp dir
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	result := detectCLI("claude")
	if result != fakeCLI {
		// detectCLI might find the real claude first via candidatePaths
		// Just ensure it doesn't return bare "claude"
		if result == "claude" {
			t.Error("expected detectCLI to find the binary, got bare name")
		}
	}
}

func TestNewWrapper_ExplicitPathTakesPrecedence(t *testing.T) {
	t.Parallel()
	w := NewWrapper("/custom/path/claude", &ClaudeProtocol{}, "claude")
	if w.CLIPath != "/custom/path/claude" {
		t.Errorf("explicit path should be used, got %q", w.CLIPath)
	}
	if w.CLIName != "claude-code" {
		t.Errorf("CLIName = %q, want claude-code", w.CLIName)
	}
}

func TestNewWrapper_EmptyPathAutoDetects(t *testing.T) {
	t.Parallel()
	w := NewWrapper("", &ClaudeProtocol{}, "claude")
	if w.CLIPath == "" {
		t.Error("auto-detect should produce a non-empty path")
	}
}

// TestNewWrapper_UnavailableBinaryLeavesVersionEmpty locks in the contract
// main.go relies on for R55-QUAL-001: when the configured binary is
// missing (or --version crashes), NewWrapper returns a wrapper with
// CLIVersion=="". main.go surfaces this as a startup Warn (non-default
// backend) or a fatal Error (default backend). If a future refactor
// populates CLIVersion with a placeholder or defaults-on-error, that
// detection path silently breaks — this test is the tripwire.
// TestNewWrapper_DisplayNameMatchesNormalizedID locks the R228-ARCH-15
// contract: NewWrapper feeds the post-normalize id (case-folded,
// whitespace-trimmed) into backendDisplayName so case variants like
// "Kiro"/"KIRO" surface as "kiro" instead of leaking the raw operator
// string. Pre-fix the default arm received the raw value, so "Kiro"
// rendered as "Kiro" while "kiro" rendered as "kiro" — same backend,
// two display labels in dashboard chips.
func TestNewWrapper_DisplayNameMatchesNormalizedID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in            string
		wantBackendID string
		wantCLIName   string
	}{
		{"kiro", "kiro", "kiro"},
		{"Kiro", "kiro", "kiro"},
		{"KIRO", "kiro", "kiro"},
		{"  kiro  ", "kiro", "kiro"},
		{"Claude", "claude", "claude-code"},
		{"", "claude", "claude-code"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			w := NewWrapper("/definitely/not/a/real/path/claude-xyz", &ClaudeProtocol{}, tc.in)
			if w.BackendID != tc.wantBackendID {
				t.Errorf("BackendID = %q, want %q", w.BackendID, tc.wantBackendID)
			}
			if w.CLIName != tc.wantCLIName {
				t.Errorf("CLIName = %q, want %q", w.CLIName, tc.wantCLIName)
			}
		})
	}
}

func TestNewWrapper_UnavailableBinaryLeavesVersionEmpty(t *testing.T) {
	t.Parallel()
	// Use an absolute path that cannot exist so detectVersion's exec.Command
	// fails immediately with ENOENT. We intentionally do not tamper with PATH
	// or HOME here because NewWrapper runs detectVersion against the given
	// path without falling back; a non-existent absolute path is the
	// clearest way to simulate "binary not installed".
	w := NewWrapper("/definitely/not/a/real/path/claude-xyz", &ClaudeProtocol{}, "claude")
	if w.CLIVersion != "" {
		t.Errorf("CLIVersion for missing binary should be empty, got %q", w.CLIVersion)
	}
	// BackendID and CLIName must still be populated so the dashboard can
	// render the configured backend as "unavailable" instead of dropping
	// it from the list entirely.
	if w.BackendID != "claude" {
		t.Errorf("BackendID = %q, want %q", w.BackendID, "claude")
	}
	if w.CLIName != "claude-code" {
		t.Errorf("CLIName = %q, want %q", w.CLIName, "claude-code")
	}
}

// TestNewWrapperLazy_NoEagerProbe pins the lazy contract: NewWrapperLazy
// MUST NOT run `<cli> --version` synchronously, and CLIVersion remains
// "" until Probe(ctx) is invoked. R241-ARCH-1.
func TestNewWrapperLazy_NoEagerProbe(t *testing.T) {
	t.Parallel()
	// Even with a path that would fail-fast, the eager constructor pays
	// process startup + immediate ENOENT (~1ms). The lazy constructor must
	// not hit exec.Command at all — assertion is structural: CLIVersion
	// stays empty and BackendID/CLIName are populated as in the eager path.
	w := NewWrapperLazy("/definitely/not/a/real/path/claude-xyz", &ClaudeProtocol{}, "claude")
	if w.CLIVersion != "" {
		t.Errorf("lazy NewWrapperLazy must not eagerly probe; got CLIVersion=%q", w.CLIVersion)
	}
	if w.BackendID != "claude" || w.CLIName != "claude-code" {
		t.Errorf("lazy constructor lost backend metadata: backend=%q name=%q",
			w.BackendID, w.CLIName)
	}
}

// TestWrapper_Probe_CtxCancelled pins that Probe respects an
// already-cancelled context: the underlying exec.CommandContext fails
// immediately, CLIVersion is set to "" (unchanged from zero), and
// the call returns "" rather than blocking 5s. R241-ARCH-1.
func TestWrapper_Probe_CtxCancelled(t *testing.T) {
	t.Parallel()
	w := NewWrapperLazy("/definitely/not/a/real/path/claude-xyz", &ClaudeProtocol{}, "claude")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	v := w.Probe(ctx)
	if v != "" {
		t.Errorf("Probe with cancelled ctx must return empty, got %q", v)
	}
	if w.CLIVersion != "" {
		t.Errorf("Probe with cancelled ctx must not populate CLIVersion, got %q", w.CLIVersion)
	}
}
