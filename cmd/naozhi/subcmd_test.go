package main

import (
	"flag"
	"strings"
	"testing"
)

// TestSubcmdRegistryDispatch pins the registry contract: every operational
// subcommand resolves to a runnable entry, unknown names do not, and the
// generated usage lists each command so `naozhi` with no / unknown args can
// print an accurate listing.
func TestSubcmdRegistryDispatch(t *testing.T) {
	names := []string{"setup", "install", "uninstall", "version", "shim", "doctor", "upgrade", "cost", "config"}
	for _, name := range names {
		sc := findSubcmd(name)
		if sc == nil {
			t.Errorf("findSubcmd(%q) = nil, want a registry entry", name)
			continue
		}
		if sc.run == nil {
			t.Errorf("subcommand %q has no run func", name)
		}
		if sc.usage == "" {
			t.Errorf("subcommand %q has no usage line", name)
		}
	}
	if len(subcmds) != len(names) {
		t.Errorf("registry has %d entries, test knows %d — update both together", len(subcmds), len(names))
	}
	for _, unknown := range []string{"", "serve", "-config", "Setup"} {
		if findSubcmd(unknown) != nil {
			t.Errorf("findSubcmd(%q) matched, want nil", unknown)
		}
	}

	var b strings.Builder
	printUsage(&b)
	out := b.String()
	for _, name := range names {
		if !strings.Contains(out, name) {
			t.Errorf("usage output lacks subcommand %q:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "usage: naozhi") {
		t.Errorf("usage output lacks the one-line usage header:\n%s", out)
	}
}

// TestNewSubFlagSet_SharedConfigFlag pins the shared -config helper: both
// default classes parse, and the returned pointer tracks the flag.
func TestNewSubFlagSet_SharedConfigFlag(t *testing.T) {
	fs, configPath := newSubFlagSet("probe", "config.yaml")
	if err := fs.Parse([]string{"-config", "/tmp/x.yaml"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *configPath != "/tmp/x.yaml" {
		t.Errorf("configPath = %q, want /tmp/x.yaml", *configPath)
	}

	fs2, configPath2 := newSubFlagSet("probe2", "")
	if err := fs2.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *configPath2 != "" {
		t.Errorf("empty-default configPath = %q, want \"\" (command resolves ~/.naozhi later)", *configPath2)
	}
}

// Every subcommand's FlagSet has to come from one place, or a new one picks its
// own error mode: with flag.ContinueOnError (the zero value) an unknown flag
// returns an error that a `_ = fs.Parse(args)` call site throws away, so
// `naozhi upgrade --no-restrat` would run a full upgrade WITH a restart instead
// of printing usage. The three hand-rolled FlagSets that predated the registry
// (upgrade, shim run, shim stop) went through this too.
func TestSubcommandFlagSets_ExitOnError(t *testing.T) {
	for _, name := range []string{"upgrade", "naozhi shim run", "naozhi shim stop"} {
		if got := newFlagSet(name).ErrorHandling(); got != flag.ExitOnError {
			t.Errorf("newFlagSet(%q) error handling = %v, want ExitOnError", name, got)
		}
	}
	// newSubFlagSet is the same half plus -config, so it must agree.
	fs, path := newSubFlagSet("config check", "config.yaml")
	if got := fs.ErrorHandling(); got != flag.ExitOnError {
		t.Errorf("newSubFlagSet error handling = %v, want ExitOnError", got)
	}
	if path == nil || *path != "config.yaml" {
		t.Errorf("newSubFlagSet must keep its -config default, got %v", path)
	}
}

// The consolidation must not have given the config-less subcommands a -config
// flag they ignore: accepting a flag that does nothing is worse than not having
// it, since an operator would believe it was honoured.
func TestConfiglessSubcommands_HaveNoConfigFlag(t *testing.T) {
	for _, name := range []string{"upgrade", "naozhi shim run", "naozhi shim stop"} {
		if f := newFlagSet(name).Lookup("config"); f != nil {
			t.Errorf("newFlagSet(%q) must not define -config", name)
		}
	}
}
