package main

// `naozhi config migrate` upgrades config.yaml to the current schema version.
//
// Deprecated keys used to be permanent: the load path rewrote nodes →
// workspaces, session.workspace → session.cwd, dropped session.auto_chain and
// lifted --append-system-prompt out of agents[].args, reported each one, and left
// every one of them on disk to be reported again next boot. This is the command
// that lets them leave.
//
// Dry by default. `-write` commits exactly the bytes the dry run printed, after
// the produced document has been re-parsed and re-validated (internal/config
// MigrateFile does that check, so a surgery bug cannot land a file the loader
// would refuse).

import (
	"fmt"
	"io"

	"github.com/naozhi/naozhi/internal/config"
)

// configMigrate implements the command. Exit codes: 2 = the file could not be
// read/parsed/migrated, 1 = a migration IS pending (dry run only, so a CI check
// can gate on it), 0 = already current, or written.
func configMigrate(args []string, stdout io.Writer) int {
	fs, configPath := newSubFlagSet("config migrate", "config.yaml")
	write := fs.Bool("write", false, "apply the migration to the file (atomic, 0600)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	res, err := config.MigrateFile(*configPath)
	if err != nil {
		fmt.Fprintf(stdout, "FATAL: %s\n", err)
		return 2
	}
	if !res.Changed() {
		fmt.Fprintf(stdout, "config migrate: already at schema_version %d, nothing to do\n", config.CurrentSchemaVersion)
		return 0
	}

	for _, a := range res.Applied {
		fmt.Fprintf(stdout, "MIGRATE: %s\n", a)
	}
	if !*write {
		// The diff is what an operator needs to see before agreeing to a
		// rewrite of a file they hand-maintain; the byte count alone is not
		// enough to spot a lost comment.
		fmt.Fprintln(stdout, "\n--- would write ---")
		_, _ = stdout.Write(res.After)
		fmt.Fprintf(stdout, "--- end (%d bytes, was %d) ---\n", len(res.After), len(res.Before))
		fmt.Fprintln(stdout, "\nconfig migrate: dry run; re-run with -write to apply")
		return 1
	}
	if err := config.WriteMigrated(*configPath, res); err != nil {
		fmt.Fprintf(stdout, "FATAL: %s\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "\nconfig migrate: wrote %s (%d bytes)\n", *configPath, len(res.After))
	return 0
}
