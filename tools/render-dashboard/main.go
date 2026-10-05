// Command render-dashboard writes the dashboard page exactly as the server
// renders it (import map, entry loaders, modulepreload links, versioned URLs)
// and the Content-Security-Policy it is served with, so the e2e mock can
// serve them (#3330). Run from the repo root:
//
//	go run ./tools/render-dashboard -out DIR
//
// It writes DIR/dashboard.html and DIR/dashboard.csp.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/naozhi/naozhi/internal/server"
)

func main() {
	out := flag.String("out", "", "directory to write dashboard.html and dashboard.csp into (required)")
	flag.Parse()
	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "render-dashboard:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	if out == "" {
		return errors.New("-out is required")
	}
	page, csp := server.DashboardPage()
	if len(page) == 0 {
		return errors.New("dashboard.html is not embedded")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "dashboard.html"), page, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "dashboard.csp"), []byte(csp), 0o644)
}
