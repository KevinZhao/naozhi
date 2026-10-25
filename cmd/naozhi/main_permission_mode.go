package main

import (
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/config"
)

// cliPermissionMode maps cli.permission_mode to the SpawnOptions value the
// router spawns, drift-checks and reports under. Config validation has already
// rejected every value but "", "bypass" and "auto".
func cliPermissionMode(cfg *config.Config) cli.PermissionMode {
	if cfg.CLI.PermissionMode == config.PermissionModeAuto {
		return cli.PermissionModeAuto
	}
	return cli.PermissionModeDefault
}
