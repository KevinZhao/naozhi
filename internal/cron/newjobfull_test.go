package cron

import "testing"

// TestNewJob_DelegatesToNewJobFull pins R250-CR-9 (#1142): NewJob is a thin
// wrapper over NewJobFull, so the (schedule, prompt, JobIMContext) path and
// the dashboard JobInit path map the shared fields identically. A future
// field-rename that touches only one of the two constructors would diverge
// here.
func TestNewJob_DelegatesToNewJobFull(t *testing.T) {
	ctx := JobIMContext{Platform: "feishu", ChatID: "c1", ChatType: "group", CreatedBy: "u1"}
	via := NewJob("0 * * * *", "hello", ctx)
	full := NewJobFull(JobInit{Schedule: "0 * * * *", Prompt: "hello", IM: ctx})

	if *via != *full {
		t.Fatalf("NewJob and NewJobFull diverged for the shared field set:\n NewJob=%+v\n full=%+v", *via, *full)
	}
	// CreatedAt must stay zero — AddJob is the stamping choke point.
	if !via.CreatedAt.IsZero() {
		t.Errorf("NewJob stamped CreatedAt=%v; want zero (AddJob owns it)", via.CreatedAt)
	}
}

// TestNewJobFull_MapsAllFields confirms every operator-settable field on
// JobInit reaches the constructed Job. This is the invariant that lets the
// dashboard create handler route through NewJobFull instead of a 14-field
// cron.Job{} literal that a rename could silently break.
func TestNewJobFull_MapsAllFields(t *testing.T) {
	notify := true
	in := JobInit{
		Schedule:       "*/5 * * * *",
		Prompt:         "scan repo",
		IM:             JobIMContext{Platform: "dashboard", ChatID: "d1", ChatType: "direct", CreatedBy: "admin"},
		Title:          "Nightly scan",
		WorkDir:        "/srv/repo",
		Backend:        "claude",
		NotifyPlatform: "feishu",
		NotifyChatID:   "n1",
		Notify:         &notify,
		FreshContext:   true,
		Paused:         true,
	}
	j := NewJobFull(in)

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"Schedule", j.Schedule, in.Schedule},
		{"Prompt", j.Prompt, in.Prompt},
		{"Platform", j.Platform, in.IM.Platform},
		{"ChatID", j.ChatID, in.IM.ChatID},
		{"ChatType", j.ChatType, in.IM.ChatType},
		{"CreatedBy", j.CreatedBy, in.IM.CreatedBy},
		{"Title", j.Title, in.Title},
		{"WorkDir", j.WorkDir, in.WorkDir},
		{"Backend", j.Backend, in.Backend},
		{"NotifyPlatform", j.NotifyPlatform, in.NotifyPlatform},
		{"NotifyChatID", j.NotifyChatID, in.NotifyChatID},
		{"FreshContext", j.FreshContext, in.FreshContext},
		{"Paused", j.Paused, in.Paused},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v; want %v", c.name, c.got, c.want)
		}
	}
	if j.Notify == nil || *j.Notify != notify {
		t.Errorf("Notify = %v; want pointer to %v", j.Notify, notify)
	}
	if !j.CreatedAt.IsZero() {
		t.Errorf("NewJobFull stamped CreatedAt=%v; want zero (AddJob owns it)", j.CreatedAt)
	}
}
