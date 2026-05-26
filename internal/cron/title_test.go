package cron

import (
	"errors"
	"strings"
	"testing"
)

// errorsIs is a thin wrapper around errors.Is so the test reads naturally.
func errorsIs(err, target error) bool { return errors.Is(err, target) }

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

// TestAddJob_PromptLengthGuard 验证 AddJob 在 scheduler 层拒绝超过
// MaxPromptBytes 的 prompt（#889 / R244-SEC-P2-5）。仅当上游
// dashboard handler 被绕过（例如测试或未来 IM 接入）时此 guard 生效。
func TestAddJob_PromptLengthGuard(t *testing.T) {
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

	huge := strings.Repeat("a", MaxPromptBytes+1)
	err := s.AddJob(&Job{
		Schedule: "@every 30m",
		Prompt:   huge,
	})
	if err == nil {
		t.Fatal("AddJob should reject prompt > MaxPromptBytes")
	}
	// Error must wrap ErrInvalidPrompt for callers using errors.Is.
	if !errorsIs(err, ErrInvalidPrompt) {
		t.Fatalf("err = %v, want errors.Is(ErrInvalidPrompt)", err)
	}
}

// TestUpdateJob_PromptLengthGuard mirrors TestAddJob_PromptLengthGuard for
// the UpdateJob path (#889).
func TestUpdateJob_PromptLengthGuard(t *testing.T) {
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

	if err := s.AddJob(&Job{Schedule: "@every 30m", Prompt: "ok"}); err != nil {
		t.Fatalf("AddJob seed: %v", err)
	}
	var seedID string
	for id := range s.jobs {
		seedID = id
		break
	}

	huge := strings.Repeat("b", MaxPromptBytes+1)
	_, err := s.UpdateJob(seedID, JobUpdate{Prompt: &huge})
	if err == nil {
		t.Fatal("UpdateJob should reject prompt > MaxPromptBytes")
	}
	if !errorsIs(err, ErrInvalidPrompt) {
		t.Fatalf("err = %v, want errors.Is(ErrInvalidPrompt)", err)
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

// TestJobUpdate_ApplyTo pins the contract for R238-ARCH-14 (#778) helper
// that replaced UpdateJob's inline `if upd.X != nil` ladder. Each
// patchable field MUST behave the same as the prior inline write:
// nil leaves the field alone, non-nil writes through. WorkDir change
// also clears LastSessionID (claude JSONL keyed by cwd). Schedule is
// intentionally NOT in scope — the helper only handles pure-data fields.
func TestJobUpdate_ApplyTo(t *testing.T) {
	t.Parallel()

	t.Run("nil_fields_are_no_ops", func(t *testing.T) {
		t.Parallel()
		original := Job{
			ID:             "j1",
			Prompt:         "orig prompt",
			WorkDir:        "/orig/wd",
			Title:          "orig title",
			Backend:        "orig-backend",
			NotifyPlatform: "feishu",
			NotifyChatID:   "chat-orig",
			FreshContext:   true,
			LastSessionID:  "sess-orig",
		}
		j := original
		var upd JobUpdate // every field nil
		upd.applyTo(&j)
		if j != original {
			t.Errorf("zero-update changed job: got %+v want %+v", j, original)
		}
	})

	t.Run("each_field_writes_through", func(t *testing.T) {
		t.Parallel()
		j := &Job{Prompt: "old"}
		newPrompt := "new prompt"
		newWorkDir := "/new/wd"
		newTitle := "new title"
		newBackend := "new-backend"
		newPlat := "telegram"
		newChat := "chat-new"
		notify := true
		fresh := false

		upd := JobUpdate{
			Prompt:         &newPrompt,
			WorkDir:        &newWorkDir,
			Title:          &newTitle,
			Backend:        &newBackend,
			NotifyPlatform: &newPlat,
			NotifyChatID:   &newChat,
			Notify:         &notify,
			FreshContext:   &fresh,
		}
		upd.applyTo(j)

		if j.Prompt != newPrompt {
			t.Errorf("Prompt = %q want %q", j.Prompt, newPrompt)
		}
		if j.WorkDir != newWorkDir {
			t.Errorf("WorkDir = %q want %q", j.WorkDir, newWorkDir)
		}
		if j.Title != newTitle {
			t.Errorf("Title = %q want %q", j.Title, newTitle)
		}
		if j.Backend != newBackend {
			t.Errorf("Backend = %q want %q", j.Backend, newBackend)
		}
		if j.NotifyPlatform != newPlat {
			t.Errorf("NotifyPlatform = %q want %q", j.NotifyPlatform, newPlat)
		}
		if j.NotifyChatID != newChat {
			t.Errorf("NotifyChatID = %q want %q", j.NotifyChatID, newChat)
		}
		if j.Notify == nil || *j.Notify != notify {
			t.Errorf("Notify = %v want %v", j.Notify, notify)
		}
		if j.FreshContext != fresh {
			t.Errorf("FreshContext = %v want %v", j.FreshContext, fresh)
		}
	})

	t.Run("workdir_change_clears_last_session_id", func(t *testing.T) {
		t.Parallel()
		j := &Job{WorkDir: "/old/wd", LastSessionID: "sess-was-here"}
		newWD := "/new/wd"
		(JobUpdate{WorkDir: &newWD}).applyTo(j)
		if j.LastSessionID != "" {
			t.Errorf("LastSessionID should clear on WorkDir change, got %q", j.LastSessionID)
		}
		if j.WorkDir != newWD {
			t.Errorf("WorkDir = %q want %q", j.WorkDir, newWD)
		}
	})

	t.Run("workdir_unchanged_keeps_last_session_id", func(t *testing.T) {
		t.Parallel()
		j := &Job{WorkDir: "/same/wd", LastSessionID: "sess-keep-me"}
		sameWD := "/same/wd"
		(JobUpdate{WorkDir: &sameWD}).applyTo(j)
		if j.LastSessionID != "sess-keep-me" {
			t.Errorf("LastSessionID should persist when WorkDir unchanged, got %q", j.LastSessionID)
		}
	})

	t.Run("notify_pointer_does_not_alias_input", func(t *testing.T) {
		// The inline ladder (and applyTo) deliberately copies the bool
		// before storing the pointer so a caller mutating its local var
		// post-Apply doesn't bleed into the persisted Job.
		t.Parallel()
		val := true
		upd := JobUpdate{Notify: &val}
		j := &Job{}
		upd.applyTo(j)

		val = false // caller mutates after applyTo
		if j.Notify == nil || *j.Notify != true {
			t.Errorf("Notify should not alias caller's pointer; got %v after caller flip", j.Notify)
		}
	})

	t.Run("clear_string_with_empty_pointer", func(t *testing.T) {
		// pointer-to-"" is the documented "clear this field" path.
		t.Parallel()
		j := &Job{Title: "old", Backend: "old-backend", NotifyPlatform: "feishu", NotifyChatID: "c"}
		empty := ""
		upd := JobUpdate{Title: &empty, Backend: &empty, NotifyPlatform: &empty, NotifyChatID: &empty}
		upd.applyTo(j)
		if j.Title != "" || j.Backend != "" || j.NotifyPlatform != "" || j.NotifyChatID != "" {
			t.Errorf("pointer-to-empty did not clear: %+v", j)
		}
	})
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
