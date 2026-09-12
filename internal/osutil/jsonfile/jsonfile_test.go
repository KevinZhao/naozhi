package jsonfile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type payload struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func opts(max int64) Options { return Options{MaxBytes: max, Label: "test store"} }

// corruptSiblings returns the .corrupt.* files next to path.
func corruptSiblings(t *testing.T, path string) []string {
	t.Helper()
	got, err := filepath.Glob(path + ".corrupt.*")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestLoadParsesAndReportsParsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	write(t, path, []byte(`{"name":"a","n":7}`))
	got, out, err := Load[payload](path, opts(1<<20))
	if err != nil || out != Parsed {
		t.Fatalf("out=%v err=%v, want Parsed/nil", out, err)
	}
	if got != (payload{Name: "a", N: 7}) {
		t.Fatalf("got %+v", got)
	}
}

// TestLoadTreatsMissingAndEmptyAsAbsent pins the two cases where starting from
// empty state destroys nothing, so neither is reported as an error and neither
// leaves a .corrupt sibling. An empty file is a half-finished write: preserving
// zero bytes as evidence would be litter.
func TestLoadTreatsMissingAndEmptyAsAbsent(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.json")
	if _, out, err := Load[payload](missing, opts(1<<20)); out != Absent || err != nil {
		t.Errorf("missing: out=%v err=%v, want Absent/nil", out, err)
	}
	empty := filepath.Join(dir, "empty.json")
	write(t, empty, nil)
	if _, out, err := Load[payload](empty, opts(1<<20)); out != Absent || err != nil {
		t.Errorf("empty: out=%v err=%v, want Absent/nil", out, err)
	}
	if got := corruptSiblings(t, empty); len(got) != 0 {
		t.Errorf("empty file left %v; nothing to preserve", got)
	}
	if _, err := os.Stat(empty); err != nil {
		t.Errorf("empty file should be left in place: %v", err)
	}
	// Also the "" path (a store with persistence disabled).
	if _, out, err := Load[payload]("", opts(1<<20)); out != Absent || err != nil {
		t.Errorf(`path "": out=%v err=%v, want Absent/nil`, out, err)
	}
}

// TestLoadPreservesCorruptFileByDefault is the invariant #673 is about: the
// unparseable bytes must survive the next atomic save.
func TestLoadPreservesCorruptFileByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	write(t, path, []byte(`{not json`))
	_, out, err := Load[payload](path, opts(1<<20))
	if err != nil || out != CorruptPreserved {
		t.Fatalf("out=%v err=%v, want CorruptPreserved/nil", out, err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("original path still exists; it should have been renamed aside")
	}
	sibs := corruptSiblings(t, path)
	if len(sibs) != 1 {
		t.Fatalf("corrupt siblings = %v, want exactly 1", sibs)
	}
	data, readErr := os.ReadFile(sibs[0])
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != `{not json` {
		t.Errorf("preserved bytes = %q, want the original", data)
	}
}

// TestLoadLeaveCorruptKeepsTheFileWhereItIs covers the uiprefs policy: nothing
// irreplaceable, so no .corrupt litter.
func TestLoadLeaveCorruptKeepsTheFileWhereItIs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	write(t, path, []byte(`{not json`))
	o := opts(1 << 20)
	o.Corrupt = LeaveCorrupt
	_, out, err := Load[payload](path, o)
	if err != nil || out != CorruptLeft {
		t.Fatalf("out=%v err=%v, want CorruptLeft/nil", out, err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("file should be left in place: %v", statErr)
	}
	if got := corruptSiblings(t, path); len(got) != 0 {
		t.Errorf("LeaveCorrupt still made siblings: %v", got)
	}
}

// TestLoadRefusesOverCapAndLeavesTheFile is the case a caller must not treat as
// empty: an oversized state file is usually real operator data, so it stays put
// and the error makes continuing the caller's explicit choice.
func TestLoadRefusesOverCapAndLeavesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	body, err := json.Marshal(payload{Name: strings.Repeat("x", 4096), N: 1})
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, body)
	_, out, loadErr := Load[payload](path, opts(int64(len(body)-1)))
	if loadErr == nil {
		t.Fatal("want an error over the cap")
	}
	if out != Absent {
		t.Errorf("out = %v; only the error should carry the refusal", out)
	}
	if !strings.Contains(loadErr.Error(), "exceeds size cap") {
		t.Errorf("err = %v, want it to name the cap", loadErr)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("over-cap file must be left in place: %v", statErr)
	}
	// Exactly at the cap is fine — the +1 read must not count against it.
	if _, out, err := Load[payload](path, opts(int64(len(body)))); err != nil || out != Parsed {
		t.Errorf("at the cap: out=%v err=%v, want Parsed/nil", out, err)
	}
}

// TestLoadRefusesSymlink pins the O_NOFOLLOW guard (#829): a symlink where a
// state file belongs is an attempted swap, so it is refused rather than followed,
// and reported distinctly from an I/O error.
func TestLoadRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	write(t, real, []byte(`{"name":"a","n":1}`))
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, out, err := Load[payload](link, opts(1<<20))
	if err == nil {
		t.Fatalf("followed the symlink: got %+v out=%v", got, out)
	}
	if !errors.Is(err, ErrSymlink) {
		t.Errorf("err = %v, want ErrSymlink", err)
	}
	if got != (payload{}) {
		t.Errorf("returned data from a refused read: %+v", got)
	}
}

// TestLoadRefusesNonRegularFile covers what O_NOFOLLOW cannot: a directory or
// fifo at the path opens fine and only Fstat catches it.
func TestLoadRefusesNonRegularFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "adir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, err := Load[payload](dir, opts(1<<20))
	if err == nil {
		t.Fatal("want an error for a directory")
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Errorf("err = %v, want it to mention regular file", err)
	}
}

// TestLoadRejectsZeroMaxBytes: unbounded is the thing this package exists to
// prevent, so a forgotten cap is a loud programming error rather than a silent
// slurp of whatever is on disk. The error must name MaxBytes: without the guard,
// MaxBytes=0 still fails — LimitReader(f, 0+1) reads one byte and the cap branch
// rejects it — so asserting "any error" would pass either way.
func TestLoadRejectsZeroMaxBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	write(t, path, []byte(`{"name":"a"}`))
	_, _, err := Load[payload](path, Options{Label: "test store"})
	if err == nil {
		t.Fatal("want an error for MaxBytes = 0")
	}
	if !strings.Contains(err.Error(), "MaxBytes") {
		t.Errorf("err = %v, want it to name MaxBytes (a zero cap is a caller bug, not an oversized file)", err)
	}
}

// TestOutcomeZeroValueIsAbsent: a caller that ignores the Outcome must never end
// up treating an unread file as parsed data.
func TestOutcomeZeroValueIsAbsent(t *testing.T) {
	var zero Outcome
	if zero != Absent {
		t.Fatalf("zero Outcome = %v, want Absent", zero)
	}
	if Absent == Parsed {
		t.Fatal("Absent and Parsed must differ")
	}
}
