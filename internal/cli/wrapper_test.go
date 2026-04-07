package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCandidatePaths_ContainsNativeInstaller(t *testing.T) {
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

func TestDetectCLI_FallsBackToBareNameWhenNothingFound(t *testing.T) {
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
	// With empty PATH and no files, should fall back to bare "claude"
	if result != "claude" {
		t.Errorf("expected bare name fallback, got %q", result)
	}
}

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
	w := NewWrapper("/custom/path/claude", &ClaudeProtocol{}, "claude")
	if w.CLIPath != "/custom/path/claude" {
		t.Errorf("explicit path should be used, got %q", w.CLIPath)
	}
	if w.CLIName != "claude-code" {
		t.Errorf("CLIName = %q, want claude-code", w.CLIName)
	}
}

func TestNewWrapper_EmptyPathAutoDetects(t *testing.T) {
	w := NewWrapper("", &ClaudeProtocol{}, "claude")
	if w.CLIPath == "" {
		t.Error("auto-detect should produce a non-empty path")
	}
}
