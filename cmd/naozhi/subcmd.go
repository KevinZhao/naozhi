// Minimal subcommand registry (#2529): adding an operational subcommand is
// one table row, not a new case in main() plus a hand-rolled FlagSet. No
// third-party CLI framework — a slice and a loop.
package main

import (
	"flag"
	"fmt"
	"io"
)

type subcmd struct {
	name  string
	usage string // one line for the generated listing
	run   func(args []string)
}

// subcmds is the dispatch table. Order is the listing order in usage output.
var subcmds = []subcmd{
	{"setup", "interactive platform setup (weixin)", runSetup},
	{"install", "install and start the system service", runInstall},
	{"uninstall", "stop and remove the system service", runUninstall},
	{"version", "print the naozhi version", func([]string) { fmt.Println(version) }},
	{"shim", "shim process control (run|stop|list)", runShim},
	{"doctor", "run health checks against a naozhi instance", runDoctor},
	{"upgrade", "self-update to the latest release", runUpgrade},
	{"cost", "cost ledger maintenance (backfill)", runCost},
}

// findSubcmd returns the registry entry for name, or nil.
func findSubcmd(name string) *subcmd {
	for i := range subcmds {
		if subcmds[i].name == name {
			return &subcmds[i]
		}
	}
	return nil
}

// printUsage writes the registry-generated command listing.
func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: naozhi <command> [flags] | naozhi -config <path>")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "commands:")
	for _, sc := range subcmds {
		fmt.Fprintf(w, "  %-10s %s\n", sc.name, sc.usage)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "run the server with: naozhi -config config.yaml")
}

// newSubFlagSet returns a subcommand FlagSet with the shared -config flag —
// the single place that flag is declared. defaultPath "" means the command
// resolves ~/.naozhi/config.yaml itself when the flag stays unset (setup /
// install); serving and reading commands default to ./config.yaml.
func newSubFlagSet(name, defaultPath string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	usage := "path to config file"
	if defaultPath == "" {
		usage = "config file path (default ~/.naozhi/config.yaml)"
	}
	configPath := fs.String("config", defaultPath, usage)
	return fs, configPath
}
