package main

import (
	"strings"
	"testing"
)

// TestSubcmdRegistryDispatch pins the registry contract: every operational
// subcommand resolves to a runnable entry, unknown names do not, and the
// generated usage lists each command so `naozhi` with no / unknown args can
// print an accurate listing.
func TestSubcmdRegistryDispatch(t *testing.T) {
	names := []string{"setup", "install", "uninstall", "version", "shim", "doctor", "upgrade", "cost"}
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
