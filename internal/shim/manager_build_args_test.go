package shim

import (
	"strings"
	"testing"
	"time"
)

// TestBuildShimArgs_BaseShape pins the argv layout produced by
// Manager.buildShimArgs (R237-CR-11 / #717). The shim subcommand parses
// these flags via flag.FlagSet in cmd/naozhi/shim.go; reordering or
// renaming them silently is a wire-format break that only surfaces when
// a freshly-deployed naozhi spawns a shim and the shim rejects "--cwd"
// because someone introduced a typo. Pinning the argv shape here means
// such a regression fails at unit-test time, not at the next prod spawn.
func TestBuildShimArgs_BaseShape(t *testing.T) {
	m := &Manager{
		bufferSize:      4096,
		maxBufBytes:     50 * 1024 * 1024,
		idleTimeout:     4 * time.Hour,
		watchdogTimeout: 30 * time.Second,
	}

	got := m.buildShimArgs(
		"feishu:direct:alice:general",
		"/tmp/sock/abc.sock",
		"/var/lib/naozhi/state/abc.json",
		"/usr/bin/claude",
		"",
		"/home/user/work",
		nil,
	)

	want := []string{
		"shim", "run",
		"--key", "feishu:direct:alice:general",
		"--socket", "/tmp/sock/abc.sock",
		"--state-file", "/var/lib/naozhi/state/abc.json",
		"--buffer-size", "4096",
		"--max-buffer-bytes", "52428800",
		"--idle-timeout", "4h0m0s",
		"--watchdog-timeout", "30s",
		"--cli-path", "/usr/bin/claude",
		"--cwd", "/home/user/work",
	}

	if !equalStrSlice(got, want) {
		t.Fatalf("argv mismatch:\n got:  %s\n want: %s", strings.Join(got, " "), strings.Join(want, " "))
	}
}

// TestBuildShimArgs_BackendAppended ensures the optional --backend flag is
// only emitted when non-empty and lands AFTER the base shape so older
// shim binaries that read flags positionally cannot mis-parse the tail.
func TestBuildShimArgs_BackendAppended(t *testing.T) {
	m := &Manager{
		bufferSize:      4096,
		maxBufBytes:     1 << 20,
		idleTimeout:     time.Hour,
		watchdogTimeout: 10 * time.Second,
	}

	withBackend := m.buildShimArgs("k", "s", "f", "/c", "kiro", "/w", nil)
	if !containsPair(withBackend, "--backend", "kiro") {
		t.Fatalf("expected --backend kiro pair: %v", withBackend)
	}

	withoutBackend := m.buildShimArgs("k", "s", "f", "/c", "", "/w", nil)
	for _, a := range withoutBackend {
		if a == "--backend" {
			t.Fatalf("empty backend should not append flag, got: %v", withoutBackend)
		}
	}
}

// TestBuildShimArgs_CLIArgsForwarded verifies each cliArg becomes its own
// "--cli-arg <value>" pair. The shim flag parser uses flag.Var to collect
// repeating --cli-arg into a slice; coalescing them on the naozhi side
// would violate that contract.
func TestBuildShimArgs_CLIArgsForwarded(t *testing.T) {
	m := &Manager{
		bufferSize:      4096,
		maxBufBytes:     1 << 20,
		idleTimeout:     time.Hour,
		watchdogTimeout: 10 * time.Second,
	}

	cliArgs := []string{"--model", "opus-4.7", "--resume", "abc-123"}
	got := m.buildShimArgs("k", "s", "f", "/c", "", "/w", cliArgs)

	// Each cliArgs entry must appear as a flag value preceded by --cli-arg.
	count := 0
	for i := 0; i < len(got)-1; i++ {
		if got[i] == "--cli-arg" {
			count++
		}
	}
	if count != len(cliArgs) {
		t.Fatalf("--cli-arg occurrences = %d, want %d (got: %v)", count, len(cliArgs), got)
	}
	for _, a := range cliArgs {
		if !contains(got, a) {
			t.Errorf("cliArg %q not forwarded; got: %v", a, got)
		}
	}
}

// equalStrSlice compares two []string by value; used in place of
// reflect.DeepEqual to keep the test free of the reflect import.
func equalStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func containsPair(s []string, k, v string) bool {
	for i := 0; i < len(s)-1; i++ {
		if s[i] == k && s[i+1] == v {
			return true
		}
	}
	return false
}
