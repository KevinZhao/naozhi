package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLintFile_ValidDoc verifies a doc with consistent body↔fact-table
// references emits no violations.
func TestLintFile_ValidDoc(t *testing.T) {
	vs, err := lintFile(filepath.Join("testdata", "valid_doc.md"))
	if err != nil {
		t.Fatalf("lintFile: %v", err)
	}
	if len(vs) != 0 {
		t.Errorf("expected 0 violations, got %d:\n%v", len(vs), vs)
	}
}

// TestLintFile_InvalidToken verifies body drift from fact-table is caught.
func TestLintFile_InvalidToken(t *testing.T) {
	vs, err := lintFile(filepath.Join("testdata", "invalid_token.md"))
	if err != nil {
		t.Fatalf("lintFile: %v", err)
	}
	if len(vs) == 0 {
		t.Fatal("expected at least 1 violation for drifted token")
	}
	found := false
	for _, v := range vs {
		if v.Rule == "token_drift" && strings.Contains(v.Token, "43") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected token_drift on '43 字段', got: %v", vs)
	}
}

// TestLintFile_MissingTable verifies a doc without fact-table markers
// emits the missing_table violation.
func TestLintFile_MissingTable(t *testing.T) {
	vs, err := lintFile(filepath.Join("testdata", "missing_table.md"))
	if err != nil {
		t.Fatalf("lintFile: %v", err)
	}
	if len(vs) == 0 {
		t.Fatal("expected missing_table violation")
	}
	found := false
	for _, v := range vs {
		if v.Rule == "missing_table" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected missing_table rule, got: %v", vs)
	}
}

// TestParseFactTable_Basic exercises the table parser directly.
func TestParseFactTable_Basic(t *testing.T) {
	doc := `<!-- fact-table:start name="t" -->

| 维度 | 值 |
|---|---|
| Hub struct 字段 | **47** |
| Server 行数 | **21313** |

<!-- fact-table:end -->`

	tbl, err := parseFactTable(doc, "in-memory")
	if err != nil {
		t.Fatalf("parseFactTable: %v", err)
	}
	if tbl.Name != "t" {
		t.Errorf("name: want 't', got %q", tbl.Name)
	}
	if v := tbl.Entries["Hub struct 字段"]; v != "47" {
		t.Errorf("Hub struct 字段: want '47', got %q", v)
	}
	if v := tbl.Entries["Server 行数"]; v != "21313" {
		t.Errorf("Server 行数: want '21313', got %q", v)
	}
}

// TestNormValue verifies value normalization handles common units.
func TestNormValue(t *testing.T) {
	cases := map[string]string{
		"47":     "47",
		"**47**": "47",
		"47 字段":  "47",
		" 47  ":  "47",
		"≤ 12":   "≤ 12",
	}
	for in, want := range cases {
		if got := normValue(in); got != want {
			t.Errorf("normValue(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLooksLikeFactValue ensures pure descriptive bolds are skipped.
//
// Phase 0-LFT v1 启发式：含数字 / ≤ / ≥ 即 fact-like。这会让 "Phase 4a"
// 这类含数字的 phase 标识也进入对账——但因为 fact-table 不会有对应 key，
// 触发的是 no_anchor warning（提示作者加白名单），不是 token_drift fail。
// v2 可加单位语义解析降低 no_anchor 噪音。
func TestLooksLikeFactValue(t *testing.T) {
	yes := []string{"47", "13 PR", "≤ 40", "21313 行"}
	no := []string{"必须", "关键", "scratchPool"}
	for _, t1 := range yes {
		if !looksLikeFactValue(t1) {
			t.Errorf("%q should be fact-like", t1)
		}
	}
	for _, t2 := range no {
		if looksLikeFactValue(t2) {
			t.Errorf("%q should NOT be fact-like (descriptive)", t2)
		}
	}
}

// TestNearestKeyContext_DoesNotCrossParagraph verifies the context window
// stops at paragraph boundary (the v1 fix).
func TestNearestKeyContext_DoesNotCrossParagraph(t *testing.T) {
	body := "This is a Hub struct **47 字段** statement.\n\nServer 包共 **21313** 行."
	// Find offset of "**21313**"
	off := strings.Index(body, "**21313**")
	if off < 0 {
		t.Fatal("test setup error: cannot find token")
	}
	ctx := nearestKeyContext(body, off)
	// ctx should NOT contain "Hub" (different paragraph)
	if strings.Contains(ctx, "Hub") {
		t.Errorf("nearestKeyContext should not cross paragraph; got %q", ctx)
	}
	// ctx should contain "Server"
	if !strings.Contains(ctx, "Server") {
		t.Errorf("nearestKeyContext lost the immediate context word; got %q", ctx)
	}
}

// TestSARIF_Smoke ensures SARIF emission doesn't panic.
func TestSARIF_Smoke(t *testing.T) {
	tmpfile, err := os.CreateTemp("", "lint-fact-table-test-*.md")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())
	tmpfile.WriteString("# Test\n\nNo fact-table here.\n")
	tmpfile.Close()

	vs := []Violation{
		{Rule: "token_drift", File: "x.md", Line: 5, Token: "43", Message: "drift"},
	}
	// Should not panic.
	emitSARIF(vs)
}
