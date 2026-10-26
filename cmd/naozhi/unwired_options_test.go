package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/config"
)

// TestServerOptions_PublicTmpReachConfig closes a plumbing gap found while
// auditing #2553: ServerOptions.PublicTmpEnabled was READ by internal/server
// but assigned NOWHERE, so the __public_tmp__ pseudo-project was unreachable no
// matter what an operator wrote in config.yaml. This asserts the key parses and
// lands on the config.
func TestServerOptions_PublicTmpReachConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	const yaml = `server:
  addr: "127.0.0.1:0"
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
	if !cfg.Projects.PublicTmp {
		t.Error("projects.public_tmp: true did not reach cfg.Projects.PublicTmp — the key is not parsed")
	}
}

// TestServerOptions_PublicTmpDefaultOff is the half that matters for existing
// deployments: omitting the key must leave it off. Defaulting to true would be
// a security regression rather than a fix.
func TestServerOptions_PublicTmpDefaultOff(t *testing.T) {
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
	if cfg.Projects.PublicTmp {
		t.Error("projects.public_tmp defaults to true — every authenticated user could read /tmp")
	}
}
