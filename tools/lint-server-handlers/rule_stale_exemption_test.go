package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureExemptions writes a real file so the existence half of rule 5 passes,
// and returns an exemptions value pointing at it with the given `until`.
func fixtureExemptions(t *testing.T, until string) *exemptions {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "big.go")
	if err := os.WriteFile(path, []byte("package p\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return &exemptions{FileSize: []exemption{{Path: path, Current: 900, Limit: 500, Until: until}}}
}

func msgOf(vs []Violation) string {
	var b strings.Builder
	for _, v := range vs {
		b.WriteString(v.Message)
		b.WriteString("\n")
	}
	return b.String()
}

// TestStaleExemption_FutureDatePasses is the negative control: an entry that has
// not expired must not be reported, or every other case here proves nothing.
func TestStaleExemption_FutureDatePasses(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	if vs := scanStaleExemption(fixtureExemptions(t, "2027-03-31"), now); len(vs) != 0 {
		t.Errorf("unexpired exemption reported:\n%s", msgOf(vs))
	}
}

// TestStaleExemption_ExpiredDateFails is the point of #2561's change. Under
// until_phase this could not be tested at all: the validity window pointed at a
// phase that ADR-001 had shelved, so nothing could ever make it expire.
func TestStaleExemption_ExpiredDateFails(t *testing.T) {
	t.Parallel()
	now := time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)
	vs := scanStaleExemption(fixtureExemptions(t, "2027-03-31"), now)
	if len(vs) != 1 {
		t.Fatalf("want 1 violation for an expired exemption, got %d:\n%s", len(vs), msgOf(vs))
	}
	for _, want := range []string{"expired on 2027-03-31", "Do not extend it silently"} {
		if !strings.Contains(vs[0].Message, want) {
			t.Errorf("message missing %q:\n%s", want, vs[0].Message)
		}
	}
}

// TestStaleExemption_MissingDateFails closes the obvious dodge: if omitting the
// field were tolerated, a new entry could be permanent again just by leaving it
// out, which is exactly what until_phase amounted to.
func TestStaleExemption_MissingDateFails(t *testing.T) {
	t.Parallel()
	vs := scanStaleExemption(fixtureExemptions(t, ""), time.Now())
	if len(vs) != 1 || !strings.Contains(vs[0].Message, "no `until:` date") {
		t.Fatalf("want a missing-date violation, got %d:\n%s", len(vs), msgOf(vs))
	}
}

// TestStaleExemption_UnparseableDateFails guards the other silent-permanence
// route: a value the parser rejects must not be treated as "no expiry".
func TestStaleExemption_UnparseableDateFails(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"2027-3-31", "31/03/2027", "Phase 5", "2027-03-31T00:00:00Z"} {
		vs := scanStaleExemption(fixtureExemptions(t, bad), time.Now())
		if len(vs) != 1 || !strings.Contains(vs[0].Message, "not a YYYY-MM-DD date") {
			t.Errorf("until %q: want an unparseable-date violation, got %d:\n%s", bad, len(vs), msgOf(vs))
		}
	}
}

// TestStaleExemption_MissingFileStillFails keeps the original behaviour: an entry
// for a deleted file is stale regardless of its date.
func TestStaleExemption_MissingFileStillFails(t *testing.T) {
	t.Parallel()
	ex := &exemptions{FileSize: []exemption{{
		Path: filepath.Join(t.TempDir(), "gone.go"), Current: 900, Limit: 500, Until: "2027-03-31",
	}}}
	vs := scanStaleExemption(ex, time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC))
	if len(vs) != 1 || !strings.Contains(vs[0].Message, "non-existent file") {
		t.Fatalf("want a missing-file violation, got %d:\n%s", len(vs), msgOf(vs))
	}
}
