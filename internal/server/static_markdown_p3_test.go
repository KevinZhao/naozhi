package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// static_markdown_p3_test.go — #2428 P3 contract tests for dashboard.js.
//
//  1. formatFileSize must be declared exactly once (a second hoisted
//     declaration silently shadowed the first) and must round-then-threshold
//     so 1048575 renders "1.0 MB" rather than "1024.0 KB"; TB tier exists.
//  2. isFileRefCandidate / splitPathLine accept go-build style `file:line:col`
//     and keep only the line for the preview jump.
//
// The behavioural cases run extracted pure functions under node (skip when
// node is absent); the declaration-count contract always runs.

// extractJSConstLine returns the single source line declaring `const name =`.
func extractJSConstLine(t *testing.T, js, name string) string {
	t.Helper()
	marker := "\nconst " + name + " = "
	i := strings.Index(js, marker)
	if i < 0 {
		t.Fatalf("dashboard.js: const %s not found", name)
	}
	rest := js[i+1:]
	end := strings.Index(rest, "\n")
	if end < 0 {
		t.Fatalf("dashboard.js: const %s has no line end", name)
	}
	return rest[:end] + "\n"
}

func TestMarkdownP3_FormatFileSizeDeclaredOnce(t *testing.T) {
	js := readDashboardJS(t)
	if n := strings.Count(js, "\nfunction formatFileSize("); n != 1 {
		t.Fatalf("formatFileSize declared %d times; want exactly 1 (hoisting makes the last win, the rest dead code)", n)
	}
}

func TestMarkdownP3_FormatFileSizeBoundaries(t *testing.T) {
	js := readDashboardJS(t)
	body := extractJSFunction(t, js, "formatFileSize")
	cases := map[string]string{
		"0":             "",
		"1":             "1 B",
		"1023":          "1023 B",
		"1024":          "1.0 KB",
		"1536":          "1.5 KB",
		"1048575":       "1.0 MB",
		"1048576":       "1.0 MB",
		"1073741823":    "1.0 GB",
		"1073741824":    "1.0 GB",
		"1099511627776": "1.0 TB",
		"2199023255552": "2.0 TB",
	}
	var inputs []string
	for k := range cases {
		inputs = append(inputs, k)
	}
	script := body + `
const out = {};
for (const n of [` + strings.Join(inputs, ",") + `]) out[String(n)] = formatFileSize(n);
process.stdout.write(JSON.stringify(out));
`
	var got map[string]string
	if err := json.Unmarshal([]byte(runNode(t, script)), &got); err != nil {
		t.Fatalf("parse node output: %v", err)
	}
	for in, want := range cases {
		if got[in] != want {
			t.Errorf("formatFileSize(%s) = %q, want %q", in, got[in], want)
		}
	}
}

func TestMarkdownP3_FileRefLineCol(t *testing.T) {
	js := readDashboardJS(t)
	script := extractJSConstLine(t, js, "FILE_REF_WITH_SLASH") +
		extractJSConstLine(t, js, "FILE_REF_BARE_WITH_LINE") +
		extractJSFunction(t, js, "isFileRefCandidate") +
		extractJSFunction(t, js, "splitPathLine") + `
const out = {
  slashCol: isFileRefCandidate('src/foo.go:42:10'),
  bareCol: isFileRefCandidate('foo.go:42:10'),
  range: isFileRefCandidate('src/foo.go:42-50'),
  time: isFileRefCandidate('12:30:45'),
  tooMany: isFileRefCandidate('src/foo.go:42:10:3'),
  split: splitPathLine('src/foo.go:42:10'),
  splitRange: splitPathLine('src/foo.go:42-50'),
  splitPlain: splitPathLine('src/foo.go'),
};
process.stdout.write(JSON.stringify(out));
`
	var got struct {
		SlashCol, BareCol, Range, Time, TooMany bool
		Split, SplitRange, SplitPlain           struct{ Path, Line string }
	}
	if err := json.Unmarshal([]byte(runNode(t, script)), &got); err != nil {
		t.Fatalf("parse node output: %v", err)
	}
	if !got.SlashCol || !got.BareCol || !got.Range {
		t.Errorf("file:line:col / range should be candidates: slash=%v bare=%v range=%v", got.SlashCol, got.BareCol, got.Range)
	}
	if got.Time || got.TooMany {
		t.Errorf("12:30:45 / four-segment should NOT be candidates: time=%v tooMany=%v", got.Time, got.TooMany)
	}
	if got.Split.Path != "src/foo.go" || got.Split.Line != "42" {
		t.Errorf("splitPathLine(file:42:10) = %+v, want path=src/foo.go line=42", got.Split)
	}
	if got.SplitRange.Path != "src/foo.go" || got.SplitRange.Line != "42-50" {
		t.Errorf("splitPathLine(file:42-50) = %+v", got.SplitRange)
	}
	if got.SplitPlain.Path != "src/foo.go" || got.SplitPlain.Line != "" {
		t.Errorf("splitPathLine(file) = %+v", got.SplitPlain)
	}
}
