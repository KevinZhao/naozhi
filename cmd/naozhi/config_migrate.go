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
// would refuse). It first keeps the original as <config>.pre-migrate-v<N> and
// prints the cp command that restores it: a migrated file declares a
// schema_version an older naozhi refuses to load.

import (
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/naozhi/naozhi/internal/config"
)

// configMigrate implements the command. Exit codes: 2 = the file could not be
// read/parsed/migrated, 1 = a migration IS pending (dry run only, so a CI check
// can gate on it), 0 = already current, or written.
func configMigrate(args []string, stdout io.Writer) int {
	fs, configPath := newSubFlagSet("config migrate", "config.yaml")
	write := fs.Bool("write", false, "apply the migration to the file (atomic, 0600; the original is kept as <config>.pre-migrate-v<N>)")
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
	for _, w := range res.Warnings {
		fmt.Fprintf(stdout, "WARN: %s\n", w)
	}
	if !*write {
		// The diff is what an operator needs to see before agreeing to a
		// rewrite of a file they hand-maintain; the byte count alone is not
		// enough to spot a lost comment.
		fmt.Fprintln(stdout, "\n--- would write ---")
		_, _ = stdout.Write(res.After)
		fmt.Fprintf(stdout, "--- end (%d bytes, was %d) ---\n", len(res.After), len(res.Before))
		fmt.Fprintln(stdout, "\nnote: the rewrite normalizes blank lines, comment alignment and indentation")
		fmt.Fprintf(stdout, "config migrate: dry run; re-run with -write to apply (the current file is kept as %s.pre-migrate-v%d)\n", *configPath, res.From)
		return 1
	}
	backup, err := config.WriteMigrated(*configPath, res)
	if backup != "" {
		fmt.Fprintf(stdout, "\nconfig migrate: backup of the original: %s\n", backup)
	}
	if err != nil {
		fmt.Fprintf(stdout, "FATAL: %s\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "config migrate: wrote %s (%d bytes)\n", *configPath, len(res.After))
	fmt.Fprintf(stdout, "to roll back (e.g. before downgrading naozhi, which refuses schema_version %d): cp -p %s %s\n",
		config.CurrentSchemaVersion, shellQuote(backup), shellQuote(*configPath))
	return 0
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote makes s one POSIX shell word, so the printed rollback command can
// be pasted as is.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
