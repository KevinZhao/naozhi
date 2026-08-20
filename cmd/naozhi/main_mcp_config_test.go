package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/config"
)

// TestResolveMCPConfigFile covers RFC cli-mcp-config §4.3.
//
// The three cases marked "G5" are the load-bearing ones: cc REFUSES TO START
// when --mcp-config names a missing file, unparseable JSON, or a document with
// no `mcpServers` object (measured, cc 2.1.235 — see main_mcp_config.go). If
// resolveMCPConfigFile returned the path in any of those cases, a single stray
// comma in the operator's MCP file would stop EVERY session from spawning.
// These assertions are what keep "MCP misconfigured" from becoming "naozhi
// down".
func TestResolveMCPConfigFile(t *testing.T) {
	dir := t.TempDir()

	write := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	valid := write("valid.json", `{"mcpServers":{"asana":{"type":"http","url":"https://example.invalid/mcp"}}}`)
	emptyServers := write("empty-servers.json", `{"mcpServers":{}}`)
	badJSON := write("bad.json", `{"mcpServers":{"x":{},}}`)
	noKey := write("nokey.json", `{}`)
	notObject := write("notobject.json", `{"mcpServers":[]}`)
	// Valid envelope, deliberately broken server definition. cc starts fine here
	// and reports the problem via system/init's mcp_server_errors[], so naozhi
	// must NOT second-guess it — validation stops at the envelope.
	badServerDef := write("badserver.json", `{"mcpServers":{"x":{"type":"bogus"}}}`)
	// Non-regular file (diff review D1). A directory stands in for the whole
	// class here because it is portable; the case that motivated the check is a
	// FIFO, where os.ReadFile would block startup forever instead of failing.
	subdir := filepath.Join(dir, "adir")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Implausibly large for an mcpServers envelope: rejected on the stat, before
	// the contents are ever read into memory. Contents are valid JSON on purpose
	// so the size check is what rejects it, not the parse.
	oversized := write("huge.json", `{"mcpServers":{}}`+strings.Repeat(" ", 1<<20))

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"valid absolute path", valid, valid},
		{"empty mcpServers object is still valid", emptyServers, emptyServers},
		{"broken server def is cc's problem, not ours", badServerDef, badServerDef},

		{"relative path rejected", "mcp.json", ""},
		{"dot-relative path rejected", "./mcp.json", ""},
		{"leading dash rejected", "-rf", ""},

		{"G5: missing file rejected", filepath.Join(dir, "nope.json"), ""},
		{"G5: unparseable JSON rejected", badJSON, ""},
		{"G5: no mcpServers key rejected", noKey, ""},
		{"G5: mcpServers not an object rejected", notObject, ""},
		{"D1: non-regular file rejected", subdir, ""},
		{"D1: oversized file rejected", oversized, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.CLI.MCPConfig = tc.in
			if got := resolveMCPConfigFile(cfg); got != tc.want {
				t.Errorf("resolveMCPConfigFile(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestResolveMCPConfigFile_PermissiveModeStillAccepted pins the deliberate
// choice in RFC cli-mcp-config §5 (review finding F4): a group/other-writable
// MCP file is a real privilege concern (whoever can write it can add a stdio
// server = arbitrary command execution in every session), but it is reported via
// slog.Warn and the path is STILL returned. Failing startup on a mode check
// would trade a security nudge for an availability regression, and a
// group-writable path can be legitimate in a multi-operator deployment.
func TestResolveMCPConfigFile_PermissiveModeStillAccepted(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "loose.json")
	if err := os.WriteFile(p, []byte(`{"mcpServers":{}}`), 0o666); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Guard against a restrictive umask silently making this a no-op test.
	if err := os.Chmod(p, 0o666); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	cfg := &config.Config{}
	cfg.CLI.MCPConfig = p
	if got := resolveMCPConfigFile(cfg); got != p {
		t.Errorf("permissive mode must warn but still return the path: got %q, want %q", got, p)
	}
}
