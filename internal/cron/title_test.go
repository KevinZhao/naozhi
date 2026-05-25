package cron

import (
	"strings"
	"testing"
)

// TestJobTitleOrFallback_ExplicitTitle 验证显式 Title 优先返回（trim 后）。
func TestJobTitleOrFallback_ExplicitTitle(t *testing.T) {
	t.Parallel()
	j := &Job{Title: "  daily-briefing  ", Prompt: "ignored"}
	if got := jobTitleOrFallback(j); got != "daily-briefing" {
		t.Fatalf("jobTitleOrFallback = %q, want %q", got, "daily-briefing")
	}
}

// TestJobTitleOrFallback_EmptyTitleFirstLine 验证 Title 为空时回退到
// Prompt 首行。
func TestJobTitleOrFallback_EmptyTitleFirstLine(t *testing.T) {
	t.Parallel()
	j := &Job{
		Prompt: "Summarize today's calendar\nand highlight anything urgent",
	}
	if got := jobTitleOrFallback(j); got != "Summarize today's calendar" {
		t.Fatalf("fallback first-line = %q, want 'Summarize today's calendar'", got)
	}
}

// TestJobTitleOrFallback_LeadingBlankLines 验证以换行开头的 Prompt 会跳
// 到首个非空行，而不是返回空串。
func TestJobTitleOrFallback_LeadingBlankLines(t *testing.T) {
	t.Parallel()
	j := &Job{Prompt: "\n\n  \nReal first line\ntail"}
	if got := jobTitleOrFallback(j); got != "Real first line" {
		t.Fatalf("fallback leading-blanks = %q, want 'Real first line'", got)
	}
}

// TestJobTitleOrFallback_LongLineTruncation 验证 Prompt 首行超过
// titleFallbackRuneLimit 时按 rune 截断并带省略号；不切断多字节字符。
func TestJobTitleOrFallback_LongLineTruncation(t *testing.T) {
	t.Parallel()
	// 80 个中文字符（每个 3 字节），超过 60-rune 限制。
	line := strings.Repeat("测试", 40) // 80 runes
	j := &Job{Prompt: line}
	got := jobTitleOrFallback(j)
	// 期望：前 60 rune + "…"
	wantRunes := []rune(line)[:titleFallbackRuneLimit]
	want := string(wantRunes) + "…"
	if got != want {
		t.Fatalf("truncation got %q (%d runes), want %q (%d runes)",
			got, len([]rune(got)), want, len([]rune(want)))
	}
}

// TestJobTitleOrFallback_EmptyJob 验证 Title 和 Prompt 都为空时返回空串
// （UI 层自行决定占位符）。
func TestJobTitleOrFallback_EmptyJob(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		j    *Job
	}{
		{"nil", nil},
		{"empty", &Job{}},
		{"whitespace", &Job{Title: "   ", Prompt: "  \n  "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobTitleOrFallback(tc.j); got != "" {
				t.Errorf("want empty string, got %q", got)
			}
		})
	}
}

// TestAddJob_TitleLengthGuard 验证 AddJob 在 scheduler 层拒绝超长 Title。
func TestAddJob_TitleLengthGuard(t *testing.T) {
	t.Parallel()

	s := NewScheduler(SchedulerConfig{
		Router:    &jitterStubRouter{}, // 复用 jitter_test.go 里的 stub
		StorePath: t.TempDir() + "/cron.json",
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	overLongTitle := strings.Repeat("a", MaxCronTitleLen+1)
	err := s.AddJob(&Job{
		Schedule: "@every 30m",
		Prompt:   "test",
		Title:    overLongTitle,
	})
	if err == nil {
		t.Fatal("AddJob should reject over-long title")
	}
	if !strings.Contains(err.Error(), "title too long") {
		t.Fatalf("error should mention title length, got %v", err)
	}
}

// TestUpdateJob_TitleOnly 验证只更新 Title 不影响其他字段。
func TestUpdateJob_TitleOnly(t *testing.T) {
	t.Parallel()

	s := NewScheduler(SchedulerConfig{
		Router:    &jitterStubRouter{},
		StorePath: t.TempDir() + "/cron.json",
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	origPrompt := "original prompt"
	origSchedule := "@every 30m"
	j := &Job{
		Schedule: origSchedule,
		Prompt:   origPrompt,
		Title:    "old-title",
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	newTitle := "new-title"
	updated, err := s.UpdateJob(j.ID, JobUpdate{Title: &newTitle})
	if err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	if updated.Title != newTitle {
		t.Errorf("Title = %q, want %q", updated.Title, newTitle)
	}
	if updated.Prompt != origPrompt {
		t.Errorf("Prompt should be unchanged, got %q want %q", updated.Prompt, origPrompt)
	}
	if updated.Schedule != origSchedule {
		t.Errorf("Schedule should be unchanged, got %q want %q", updated.Schedule, origSchedule)
	}
}

// TestUpdateJob_ClearTitle 验证 Title 设为 "" 可以清空（UI 回退到 prompt fallback）。
func TestUpdateJob_ClearTitle(t *testing.T) {
	t.Parallel()

	s := NewScheduler(SchedulerConfig{
		Router:    &jitterStubRouter{},
		StorePath: t.TempDir() + "/cron.json",
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	j := &Job{Schedule: "@every 30m", Prompt: "p", Title: "existing"}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	empty := ""
	updated, err := s.UpdateJob(j.ID, JobUpdate{Title: &empty})
	if err != nil {
		t.Fatalf("UpdateJob clear: %v", err)
	}
	if updated.Title != "" {
		t.Errorf("Title should be cleared, got %q", updated.Title)
	}
	// jobTitleOrFallback 现在应该回退到 prompt fallback
	if got := jobTitleOrFallback(updated); got != "p" {
		t.Errorf("fallback after clear = %q, want %q", got, "p")
	}
}

// TestUpdateJob_TitleLengthGuard 验证 UpdateJob 也拒绝超长 Title。
func TestUpdateJob_TitleLengthGuard(t *testing.T) {
	t.Parallel()

	s := NewScheduler(SchedulerConfig{
		Router:    &jitterStubRouter{},
		StorePath: t.TempDir() + "/cron.json",
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	j := &Job{Schedule: "@every 30m", Prompt: "p"}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	overLong := strings.Repeat("a", MaxCronTitleLen+1)
	_, err := s.UpdateJob(j.ID, JobUpdate{Title: &overLong})
	if err == nil {
		t.Fatal("UpdateJob should reject over-long title")
	}
	if !strings.Contains(err.Error(), "title too long") {
		t.Fatalf("error should mention title length, got %v", err)
	}
}

// TestFormatCronNotice_StripsBidi locks the R239-SEC-5 contract: a label
// carrying bidi/directional-isolate runes must not survive into the IM
// notice. Without the SanitizeForLog pass in formatCronNotice, an
// attacker who set Job.Title via the dashboard PATCH could plant a U+202E
// (Right-to-Left Override) and reverse the rendered notice — Title isn't
// validated for control runes (only length).
func TestFormatCronNotice_StripsBidi(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rune rune
	}{
		{"RLO", 0x202E},
		{"LRE", 0x202A},
		{"RLI", 0x2067},
		{"PDI", 0x2069},
		{"LS", 0x2028},
		{"PS", 0x2029},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			label := "good" + string(tc.rune) + "evil"
			got := formatCronNotice(label, "body")
			if strings.ContainsRune(got, tc.rune) {
				t.Fatalf("notice contains %q rune U+%04X: %q", tc.name, tc.rune, got)
			}
			// Body must remain intact — only the label is sanitised at
			// this layer (success path's body is already sanitised
			// upstream via sanitiseRunResult; the static error
			// templates are clean ASCII so SanitizeForLog would no-op).
			if !strings.Contains(got, "body") {
				t.Fatalf("notice lost body suffix: %q", got)
			}
		})
	}
}
