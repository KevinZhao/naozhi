package osutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandHome_NoTilde(t *testing.T) {
	t.Parallel()
	input := "/absolute/path"
	got := ExpandHome(input)
	if got != input {
		t.Errorf("ExpandHome(%q) = %q, want %q", input, got, input)
	}
}

func TestExpandHome_RelativePath(t *testing.T) {
	t.Parallel()
	input := "relative/path"
	got := ExpandHome(input)
	if got != input {
		t.Errorf("ExpandHome(%q) = %q, want %q", input, got, input)
	}
}

func TestExpandHome_TildeSlash(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir:", err)
	}
	input := "~/some/subdir"
	got := ExpandHome(input)
	want := filepath.Join(home, "some/subdir")
	if got != want {
		t.Errorf("ExpandHome(%q) = %q, want %q", input, got, want)
	}
}

func TestExpandHome_TildeOnly(t *testing.T) {
	t.Parallel()
	// "~" alone (without slash) should NOT be expanded per the HasPrefix("~/") guard
	input := "~"
	got := ExpandHome(input)
	if got != input {
		t.Errorf("ExpandHome(%q) = %q, want %q (tilde alone should not expand)", input, got, input)
	}
}

func TestExpandHome_TildePrefix(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir:", err)
	}
	got := ExpandHome("~/")
	// filepath.Join strips trailing slash
	if !strings.HasPrefix(got, home) {
		t.Errorf("ExpandHome(\"~/\") = %q, expected prefix %q", got, home)
	}
}

func TestExpandHome_EmptyString(t *testing.T) {
	t.Parallel()
	got := ExpandHome("")
	if got != "" {
		t.Errorf("ExpandHome(\"\") = %q, want empty", got)
	}
}
