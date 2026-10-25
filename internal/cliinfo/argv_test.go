package cliinfo

import "testing"

// TestIsDeniedExtraFlag covers the exported predicate the config loader uses
// to warn on denied flags under cli.args / cli.backends[].args / agents[].args.
func TestIsDeniedExtraFlag(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"--append-system-prompt":   true,
		"--append-system-prompt=x": true,
		"--effort":                 true,
		"--model=opus":             true,
		"--mcp-config":             true,
		"--debug":                  false,
		"--keep":                   false,
		"append-system-prompt":     false,
		"-p":                       false,
		"":                         false,
	}
	for arg, want := range cases {
		if got := IsDeniedExtraFlag(arg); got != want {
			t.Errorf("IsDeniedExtraFlag(%q) = %v, want %v", arg, got, want)
		}
	}
}
