package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/server"
)

func TestRunWritesRenderedPageAndCSP(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	if err := run(dir); err != nil {
		t.Fatalf("run: %v", err)
	}
	wantPage, wantCSP := server.DashboardPage()
	page, err := os.ReadFile(filepath.Join(dir, "dashboard.html"))
	if err != nil {
		t.Fatal(err)
	}
	csp, err := os.ReadFile(filepath.Join(dir, "dashboard.csp"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(page, wantPage) {
		t.Error("dashboard.html differs from server.DashboardPage()")
	}
	if string(csp) != wantCSP {
		t.Errorf("dashboard.csp = %q, want %q", csp, wantCSP)
	}
	// The raw page has neither; writing it would let the e2e spec boot the
	// page the mock already serves and prove nothing.
	if n := bytes.Count(page, []byte(`<script type="importmap">`)); n != 1 {
		t.Errorf("page has %d import maps, want 1", n)
	}
	if !bytes.Contains(page, []byte(`<link rel="modulepreload" href="/static/`)) {
		t.Error("page has no modulepreload link")
	}
}

func TestRunRequiresOut(t *testing.T) {
	if err := run(""); err == nil || !strings.Contains(err.Error(), "-out") {
		t.Fatalf("run(\"\") = %v, want an error naming -out", err)
	}
}

func TestDashboardPageReturnsACopy(t *testing.T) {
	a, _ := server.DashboardPage()
	a[0] ^= 0xff
	b, _ := server.DashboardPage()
	if a[0] == b[0] {
		t.Fatal("mutating one DashboardPage result changed the next")
	}
}
