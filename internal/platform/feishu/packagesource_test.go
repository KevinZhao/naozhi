package feishu

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// packageSource concatenates the package's non-test Go sources so a source-level
// gate scans the PACKAGE rather than one file. Two such gates used to read
// "feishu.go" by name; when J10 (#2548) split that 1,117-line file, both failed —
// correctly, since they fatal when the function they check is absent rather than
// passing vacuously, but for the wrong reason. Scanning the package means a
// future move cannot break them and cannot make them silently trivial either.
func packageSource(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var b strings.Builder
	files := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		b.Write(data)
		b.WriteString("\n")
		files++
	}
	if files == 0 {
		t.Fatal("no non-test sources found; a source-level gate would pass vacuously")
	}
	return b.String()
}
