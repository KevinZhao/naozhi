package server

import (
	"os"
	"strings"
	"testing"
)

// readDashboardJS returns every dashboard module concatenated, so a source
// assertion stays valid whichever module a helper moves to. Each file must be
// in the embedded asset table; a missing entry is a Fatal.
func readDashboardJS(t *testing.T) string {
	t.Helper()
	var b []byte
	for _, name := range dashboardModules(t) {
		data := staticAssetBytes(name)
		if data == nil {
			t.Fatalf("static/%s is not in the embedded asset table (static_assets.go)", name)
		}
		b = append(append(b, data...), '\n')
	}
	return string(b)
}

// dashboardModules lists static/*.js except sw.js, sorted. It fails the test
// when the directory cannot be read or holds suspiciously few modules, so a
// wrong working directory cannot pass every scan vacuously.
func dashboardModules(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("static")
	if err != nil {
		t.Fatalf("read static/: %v", err)
	}
	var names []string
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && strings.HasSuffix(n, ".js") && n != "sw.js" {
			names = append(names, n)
		}
	}
	if len(names) < 20 {
		t.Fatalf("static/ holds %d dashboard modules, want at least 20: %v", len(names), names)
	}
	return names // os.ReadDir sorts by name
}

// rawDashboardHTML is dashboard.html as written; staticAssets holds the
// rendered page, whose URLs carry ?v= and whose head carries generated tags.
func rawDashboardHTML(t *testing.T) []byte {
	t.Helper()
	data, err := staticFS.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read embedded dashboard.html: %v", err)
	}
	return data
}
