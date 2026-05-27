package cron

import (
	"fmt"
	"testing"
)

// TestFormatCronNotice_MatchesPrefixFmtTemplate pins R247-PERF-7 (#539):
// the strings.Builder rewrite of formatCronNotice must produce byte-identical
// output to the original cronNoticePrefixFmt template "[Cron %s] %s". The
// two notice_label_bracket tests already pin the SECURE post-substitution
// shape, but only for two specific input cases. This test exercises a
// matrix of inputs (empty, plain, multi-byte CJK, embedded spaces) against
// the documented fmt.Sprintf template so a future micro-optimisation cannot
// silently drift the wire format.
//
// The point of this contract is anti-regression: if someone later adds a
// trailing newline / strips a space / reorders the template, the IM
// channels' rendering breaks subtly (extra blank line in Feishu cards,
// mid-line "[Cron]" prefix orphaned from body in Slack, etc.) — exactly
// the kind of bug grep-based fixture pinning misses.
func TestFormatCronNotice_MatchesPrefixFmtTemplate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		label string
		body  string
	}{
		{name: "empty-both", label: "", body: ""},
		{name: "ascii-only", label: "daily-review", body: "succeeded"},
		{name: "cjk-label", label: "每日复盘", body: "执行成功。"},
		{name: "spaces-in-body", label: "weekly", body: "this is a longer body string"},
		{name: "multiline-body", label: "x", body: "line1\nline2\nline3"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatCronNotice(tc.label, tc.body)
			// Reference rendering using the exact documented template.
			// Note: formatCronNotice ALSO substitutes `]` in label per
			// R250-SEC-6, so the reference must mirror that step to keep
			// this comparison byte-equal on labels containing `]`. None
			// of the test cases above contain `]`, so the reference and
			// formatter agree for these inputs without extra handling.
			want := fmt.Sprintf(cronNoticePrefixFmt, tc.label, tc.body)
			if got != want {
				t.Errorf("formatCronNotice(%q, %q) = %q; want %q (template %q)",
					tc.label, tc.body, got, want, cronNoticePrefixFmt)
			}
		})
	}
}

// BenchmarkFormatCronNotice gives a quick reference baseline so later
// optimisations / regressions show up against a stable measurement.
// Run via `go test -bench=BenchmarkFormatCronNotice ./internal/cron/`.
func BenchmarkFormatCronNotice(b *testing.B) {
	label := "daily-review"
	body := "this run took 1234ms and produced a 4096-byte transcript"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = formatCronNotice(label, body)
	}
}
