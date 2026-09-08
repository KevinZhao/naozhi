package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/config"
)

// TestServerOptions_DebugModeAndPublicTmpReachConfig closes a plumbing gap found
// while auditing #2553: ServerOptions.DebugMode and ServerOptions.PublicTmpEnabled
// were READ by internal/server (server.go, build_handlers.go) but assigned
// NOWHERE in the repo. No config key existed, so /api/debug/pprof,
// /api/debug/vars and the __public_tmp__ pseudo-project were unreachable no
// matter what an operator wrote in config.yaml.
//
// Worse than a missing feature, both were recorded as done: R244-SEC-P3-1's
// review entry says debug_mode shipped "来自 ServerOptions.DebugMode →
// config.yaml server.debug_mode", and three separate items (R242-SEC-6,
// R244-SEC-P3-2, R245-SEC-7) specified public_tmp as an operator opt-in flag.
// Only the struct field ever existed.
//
// This test asserts the keys parse and land on the config, so a future reader
// does not have to re-derive that the wiring is real. Defaults stay false, which
// is why adding them changes nothing for existing deployments.
func TestServerOptions_DebugModeAndPublicTmpReachConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	const yaml = `server:
  addr: "127.0.0.1:0"
  debug_mode: true
projects:
  root: /tmp/nz-projects
  public_tmp: true
cli:
  backend: claude
  path: /usr/bin/claude
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if !cfg.Server.DebugMode {
		t.Error("server.debug_mode: true did not reach cfg.Server.DebugMode — the key is not parsed")
	}
	if !cfg.Projects.PublicTmp {
		t.Error("projects.public_tmp: true did not reach cfg.Projects.PublicTmp — the key is not parsed")
	}
}

// TestServerOptions_DebugModeAndPublicTmpDefaultOff is the half that matters for
// existing deployments: omitting both keys must leave both off. Either one
// defaulting to true would be a security regression rather than a fix.
func TestServerOptions_DebugModeAndPublicTmpDefaultOff(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	const yaml = `server:
  addr: "127.0.0.1:0"
cli:
  backend: claude
  path: /usr/bin/claude
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Server.DebugMode {
		t.Error("server.debug_mode defaults to true — /api/debug/* would be registered on every deployment")
	}
	if cfg.Projects.PublicTmp {
		t.Error("projects.public_tmp defaults to true — every authenticated user could read /tmp")
	}
}
