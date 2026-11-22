package patrol

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// finding is a contract finding as JSON, its fields overridable.
func finding(id string, over map[string]string) string {
	f := map[string]string{
		"id": `"` + id + `"`, "title": `"重连时会话为空会崩"`, "kind": `"bug"`, "evidence": `["a.go:2"]`,
		"impact": `"重连即崩"`, "fix": `"先判空"`, "size": `"S"`, "reversible": "true", "confidence": `"high"`,
	}
	for k, v := range over {
		f[k] = v
	}
	var parts []string
	for _, k := range []string{"id", "title", "kind", "evidence", "impact", "fix", "size", "reversible", "confidence"} {
		if v, ok := f[k]; ok && v != "" {
			parts = append(parts, `"`+k+`":`+v)
		}
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func findings(fs ...string) string { return `{"findings":[` + strings.Join(fs, ",") + `]}` }

// repoDir is a directory with a three-line file a.go, a one-line file with
// no newline at its end, a directory, and beside it a file outside.
func repoDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "repo")
	for path, body := range map[string]string{
		filepath.Join(dir, "a.go"):            "package a\n\nfunc A() {}\n",
		filepath.Join(dir, "sub", "one.txt"):  "one line",
		filepath.Join(dir, "sub", "空 格.md"):   "x\ny\n",
		filepath.Join(root, "outside.txt"):    "secret\n",
		filepath.Join(dir, "sub", "empty.go"): "",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "outside.txt"), filepath.Join(dir, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "a.go"), filepath.Join(dir, "alias.go")); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestParseFindingsContract: a reply that is not the contract's one JSON
// object fails the run whole.
func TestParseFindingsContract(t *testing.T) {
	dir := repoDir(t)
	for name, reply := range map[string]string{
		"prose":         "没有发现问题。",
		"fenced":        "```json\n" + findings() + "\n```",
		"array":         `[{"id":"x"}]`,
		"unknown field": `{"findings":[],"note":"x"}`,
		"unknown inner": findings(`{"id":"bug-a-b","title":"t","severity":"high"}`),
		"trailing":      findings() + "\n以上。",
		"no findings":   `{}`,
		"null findings": `{"findings":null}`,
		"too long":      `{"findings":[` + strings.Repeat(" ", maxOutputBytes) + `]}`,
		"not json":      `{"findings":[`,
	} {
		if kept, _, err := ParseFindings(reply, dir); err == nil {
			t.Errorf("%s: parsed (%d kept), want an error", name, len(kept))
		}
	}
	kept, dropped, err := ParseFindings("\n  "+findings()+"\n", dir)
	if err != nil || len(kept) != 0 || dropped != 0 {
		t.Errorf("no findings = %v, %d, %v; want none and no error", kept, dropped, err)
	}
}

// TestParseFindingsEach: a finding that is not one is dropped alone and
// counted; what is only too long is cut.
func TestParseFindingsEach(t *testing.T) {
	dir := repoDir(t)
	long := strings.Repeat("长", 600)
	bad := []string{
		finding("Bug-A", nil),                                    // upper case
		finding("ab", nil),                                       // too short
		finding("bug_a_b", nil),                                  // underscore
		finding("-bug-a", nil),                                   // leading dash
		finding(strings.Repeat("a", 61), nil),                    // too long
		finding("bug-kind", map[string]string{"kind": `"perf"`}), // unknown kind
		finding("bug-size", map[string]string{"size": `"XL"`}),
		finding("bug-conf", map[string]string{"confidence": `"certain"`}),
		finding("bug-title", map[string]string{"title": `"  "`}),
		finding("bug-impact", map[string]string{"impact": `""`}),
		finding("bug-fix", map[string]string{"fix": ""}),
		finding("bug-noev", map[string]string{"evidence": `[]`}),
		finding("bug-dup", nil),
	}
	reply := findings(append([]string{finding("bug-dup", map[string]string{"title": `"` + long + `"`, "impact": `"` + long + `"`, "fix": `"` + long + `"`})}, bad...)...)
	// Thirteen bad ones after one good: past MaxFindings, so cut the list
	// to what is looked at.
	kept, dropped, err := ParseFindings(reply, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].ID != "bug-dup" {
		t.Fatalf("kept %+v, want only the first bug-dup", kept)
	}
	// 14 in all: 4 past the cap, 9 of the first ten dropped one by one.
	if dropped != 13 {
		t.Errorf("dropped %d, want 13", dropped)
	}
	f := kept[0]
	if n := len([]rune(f.Title)); n != titleCap {
		t.Errorf("title is %d runes, want cut to %d", n, titleCap)
	}
	if n := len([]rune(f.Impact)); n != impactRunes {
		t.Errorf("impact is %d runes, want cut to %d", n, impactRunes)
	}
	if n := len([]rune(f.Fix)); n != fixRunes {
		t.Errorf("fix is %d runes, want cut to %d", n, fixRunes)
	}
	// Each of the bad ones alone is dropped, whatever its place in the list.
	for i, b := range bad[:len(bad)-1] {
		if kept, dropped, err := ParseFindings(findings(b), dir); err != nil || len(kept) != 0 || dropped != 1 {
			t.Errorf("bad[%d] %s: kept %d, dropped %d, err %v", i, b, len(kept), dropped, err)
		}
	}
}

// TestParseFindingsCap: the first MaxFindings are kept, the rest counted.
func TestParseFindingsCap(t *testing.T) {
	dir := repoDir(t)
	var fs []string
	for i := range 13 {
		fs = append(fs, finding("bug-n-"+string(rune('a'+i)), nil))
	}
	kept, dropped, err := ParseFindings(findings(fs...), dir)
	if err != nil || len(kept) != MaxFindings || dropped != 3 {
		t.Fatalf("kept %d, dropped %d, err %v; want %d and 3", len(kept), dropped, err, MaxFindings)
	}
	if kept[0].ID != "bug-n-a" || kept[9].ID != "bug-n-j" {
		t.Errorf("kept %s … %s, want the first ten in order", kept[0].ID, kept[9].ID)
	}
}

// TestEvidence: the gateway opens the evidence again. What it cannot open
// inside the workspace is dropped, and a finding left with none goes too.
func TestEvidence(t *testing.T) {
	dir := repoDir(t)
	for _, tc := range []struct {
		evidence string
		ok       bool
	}{
		{"a.go:1", true},
		{"a.go:3", true},
		{"a.go:1-3", true},
		{"./a.go:2", true},
		{"sub/../a.go:2", true},
		{"sub/one.txt:1", true}, // a last line with no newline is a line
		{"sub/空 格.md:2", true},
		{"alias.go:3", true}, // a symlink that stays inside
		// A link is no evidence: the user's links are the gateway's own.
		{"https://github.com/owner/repo/issues/12", false},
		{"http://example.com/x", false},
		{"https://example.com/a.go:1", false},
		{"a.go:4", false},   // past the end
		{"a.go:2-9", false}, // range past the end
		{"a.go:3-1", false}, // backwards
		{"a.go:0", false},
		{"a.go", false}, // no line
		{"a.go:x", false},
		{"missing.go:1", false},
		{"sub:1", false},            // a directory
		{"sub/empty.go:1", false},   // no line at all
		{"../outside.txt:1", false}, // leaves the workspace
		{"sub/../../outside.txt:1", false},
		{"leak.txt:1", false},                      // a symlink pointing outside
		{filepath.Join(dir, "a.go") + ":1", false}, // absolute
		{"/etc/hosts:1", false},
		{"/a.go:1", false}, // absolute, though joined to the workspace it would be a.go
		{"ftp://example.com/x", false},
		{"https://:1", false},
		{"", false},
	} {
		kept, dropped, err := ParseFindings(findings(finding("bug-ev-x", map[string]string{"evidence": `["` + tc.evidence + `"]`})), dir)
		if err != nil {
			t.Fatalf("%q: %v", tc.evidence, err)
		}
		if got := len(kept) == 1; got != tc.ok || dropped != map[bool]int{true: 0, false: 1}[tc.ok] {
			t.Errorf("evidence %q: kept %v (dropped %d), want %v", tc.evidence, got, dropped, tc.ok)
		}
	}
	// The evidence that opens is kept, the rest of the same finding dropped,
	// a repeat listed once and no more than five in all.
	kept, _, err := ParseFindings(findings(finding("bug-ev-mix", map[string]string{
		"evidence": `["missing.go:1","a.go:2","../outside.txt:1","a.go:2","https://example.com/a","a.go:1","a.go:3","sub/one.txt:1","sub/空 格.md:1"]`})), dir)
	if err != nil || len(kept) != 1 {
		t.Fatalf("kept %v, err %v", kept, err)
	}
	if want := []string{"a.go:2", "a.go:1", "a.go:3", "sub/one.txt:1", "sub/空 格.md:1"}; !slices.Equal(kept[0].Evidence, want) {
		t.Errorf("evidence %q, want %q", kept[0].Evidence, want)
	}
	// A workspace directory that is gone opens nothing.
	if kept, dropped, err := ParseFindings(findings(finding("bug-ev-x", nil)), filepath.Join(dir, "nope")); err != nil || len(kept) != 0 || dropped != 1 {
		t.Errorf("missing directory: kept %d, dropped %d, err %v", len(kept), dropped, err)
	}
}

func TestRef(t *testing.T) {
	ws, id, ok := SplitRef(Ref("ws_ab12cd34", "bug-router-nil"))
	if !ok || ws != "ws_ab12cd34" || id != "bug-router-nil" {
		t.Errorf("SplitRef = %q, %q, %v", ws, id, ok)
	}
	for _, bad := range []string{"", "bug-router-nil", "ws_ab12cd34/", "ws_ab12cd34/Bug", "ws_AB12cd34/bug-a-b", "ws_ab12cd34/bug-a-b/c", "../ws_ab12cd34/bug-a-b", "ws_ab12cd3/bug-a-b"} {
		if _, _, ok := SplitRef(bad); ok {
			t.Errorf("SplitRef(%q) ok", bad)
		}
	}
	if got := ItemSource("ws_ab12cd34/bug-a-b"); got != "patrol:ws_ab12cd34/bug-a-b" {
		t.Errorf("ItemSource = %q", got)
	}
	// A finding's item is no deterministic check's: no round closes it.
	if g, _ := groupOf(ItemSource("ws_ab12cd34/bug-a-b")); g != "" {
		t.Errorf("groupOf(a finding's source) = %q, want none", g)
	}
}

func TestBlobLinks(t *testing.T) {
	sha := strings.Repeat("a", 40)
	got := blobLinks("owner/repo", sha, "", []string{"internal/a.go:12", "docs/空 格.md:3-9", "https://example.com/x", "bad"})
	want := []string{
		"https://github.com/owner/repo/blob/" + sha + "/internal/a.go#L12",
		"https://github.com/owner/repo/blob/" + sha + "/docs/%E7%A9%BA%20%E6%A0%BC.md#L3-L9",
	}
	if !slices.Equal(got, want) {
		t.Errorf("links %q, want %q", got, want)
	}
	// A workspace below the repository's top links by the path from the top.
	if got := blobLinks("owner/repo", sha, "services/api/", []string{"main.go:1"}); !slices.Equal(got, []string{"https://github.com/owner/repo/blob/" + sha + "/services/api/main.go#L1"}) {
		t.Errorf("links below the top: %q", got)
	}
	if blobLinks("", sha, "", []string{"a.go:1"}) != nil || blobLinks("owner/repo", "", "", []string{"a.go:1"}) != nil {
		t.Error("links without a GitHub repository or a commit")
	}
}
