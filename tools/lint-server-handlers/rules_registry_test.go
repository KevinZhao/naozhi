package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestRuleIDs_MatchEmittedViolations pins ruleIDs to the rules this directory
// can actually emit: every `Rule: "<id>"` literal in the non-test sources must
// be in ruleIDs, and every ruleIDs entry must be emitted somewhere. The SARIF
// header used to hand-list rules and drifted to name two deleted ones while
// missing send_engine_ownership (#2636); generating it from ruleIDs only helps
// if ruleIDs itself cannot drift.
func TestRuleIDs_MatchEmittedViolations(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`Rule:\s*"([a-z_]+)"`)
	emitted := map[string]bool{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		src, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			emitted[m[1]] = true
		}
	}
	if len(emitted) == 0 {
		t.Fatal("no Rule: literals found — regex drift, this test checks nothing")
	}
	listed := map[string]bool{}
	for _, id := range ruleIDs {
		listed[id] = true
	}
	for id := range emitted {
		if !listed[id] {
			t.Errorf("rule %q is emitted but missing from ruleIDs (SARIF consumers will not see its metadata)", id)
		}
	}
	for id := range listed {
		if !emitted[id] {
			t.Errorf("ruleIDs lists %q but nothing emits it — a deleted rule left in the metadata", id)
		}
	}
}

// TestSARIFReport_RulesFromRegistry: the driver.rules[] block is exactly
// ruleIDs, in order, and the document is valid JSON.
func TestSARIFReport_RulesFromRegistry(t *testing.T) {
	doc := sarifReport([]Violation{{Rule: "file_size", File: "x.go", Line: 3, Message: "m"}})
	var parsed struct {
		Runs []struct {
			Tool struct {
				Driver struct {
					Rules []struct {
						ID string `json:"id"`
					} `json:"rules"`
				} `json:"driver"`
			} `json:"tool"`
			Results []struct {
				RuleID string `json:"ruleId"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("SARIF is not valid JSON: %v\n%s", err, doc)
	}
	rules := parsed.Runs[0].Tool.Driver.Rules
	if len(rules) != len(ruleIDs) {
		t.Fatalf("rules[] has %d entries, ruleIDs has %d", len(rules), len(ruleIDs))
	}
	for i, r := range rules {
		if r.ID != ruleIDs[i] {
			t.Errorf("rules[%d] = %q, want %q", i, r.ID, ruleIDs[i])
		}
	}
	for _, dead := range []string{"iface_match", "api_route_owner"} {
		if strings.Contains(doc, dead) {
			t.Errorf("SARIF still names deleted rule %q", dead)
		}
	}
	if parsed.Runs[0].Results[0].RuleID != "file_size" {
		t.Errorf("result ruleId = %q", parsed.Runs[0].Results[0].RuleID)
	}
}

// TestFileSize_InflatedBaselineIsViolation pins the reverse ratchet (#2636): an
// exemption whose `current:` sits more than baselineSlack lines above the file
// fails, because that gap is growth CI would wave through.
func TestFileSize_InflatedBaselineIsViolation(t *testing.T) {
	dir := t.TempDir()
	// 120 lines: over the 100 limit used below, so the exemption path runs.
	body := strings.Repeat("// line\n", 120)
	path := filepath.Join(dir, "big.go")
	if err := os.WriteFile(path, []byte("package p\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	rel := filepath.ToSlash(path)
	lines := 121

	// Baseline within slack: clean.
	ok := map[string]exemption{rel: {Path: rel, Current: lines + baselineSlack, Limit: 100, Until: "2999-01-01"}}
	if vs := scanFileSize(dir, 100, ok); len(vs) != 0 {
		t.Fatalf("baseline exactly at slack reported %d violation(s): %+v", len(vs), vs)
	}
	// One past slack: violation naming the gap.
	inflated := map[string]exemption{rel: {Path: rel, Current: lines + baselineSlack + 1, Limit: 100, Until: "2999-01-01"}}
	vs := scanFileSize(dir, 100, inflated)
	if len(vs) != 1 || !strings.Contains(vs[0].Message, "re-sample `current:`") {
		t.Fatalf("want one re-sample violation, got %d: %+v", len(vs), vs)
	}
	// Growth past baseline still fires (the original direction).
	grown := map[string]exemption{rel: {Path: rel, Current: lines - 1, Limit: 100, Until: "2999-01-01"}}
	vs = scanFileSize(dir, 100, grown)
	if len(vs) != 1 || !strings.Contains(vs[0].Message, "file grew") {
		t.Fatalf("want one 'file grew' violation, got %d: %+v", len(vs), vs)
	}
}
