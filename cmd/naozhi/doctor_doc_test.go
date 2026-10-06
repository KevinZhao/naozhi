package main

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var doctorDocRow = regexp.MustCompile("(?m)^\\| `([^`]+)` \\|")

// TestDoctorDoc_TableListsEveryCategory: the check table in docs/ops/doctor.md
// is the operator's list of what doctor checks, so every category a full run
// emits needs a row, and a row no check emits is stale.
func TestDoctorDoc_TableListsEveryCategory(t *testing.T) {
	// run() includes checkStateDir, which writes a probe file under ~/.naozhi.
	t.Setenv("HOME", t.TempDir())
	d := &doctor{addr: "http://127.0.0.1:1", timeout: 200 * time.Millisecond, out: io.Discard, json: true,
		configPath: filepath.Join(t.TempDir(), "missing.yaml")}
	d.run()
	emitted := map[string]bool{}
	for _, f := range d.findings {
		c := f.Category
		if strings.HasPrefix(c, "cli backend ") {
			c = "cli backend"
		}
		emitted[c] = true
	}
	if len(emitted) < 15 {
		t.Fatalf("run() emitted %d categories, want every check represented: %+v", len(emitted), d.findings)
	}

	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "ops", "doctor.md"))
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, m := range doctorDocRow.FindAllStringSubmatch(string(doc), -1) {
		documented[strings.TrimSuffix(m[1], " <id>")] = true
	}
	for c := range emitted {
		if !documented[c] {
			t.Errorf("doctor emits category %q but docs/ops/doctor.md has no table row for it", c)
		}
	}
	for c := range documented {
		if !emitted[c] {
			t.Errorf("docs/ops/doctor.md has a row for %q, which no doctor check emits", c)
		}
	}
}
