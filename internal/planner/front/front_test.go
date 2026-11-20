package front

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func ptr(s string) *string { return &s }

// A front is made by its first update, changed field by field by the later
// ones, and survives a reload; closing takes it off the open list and a
// later update reopens it.
func TestApply(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "fronts.json")
	s := New(path, func() time.Time { return now })

	if _, err := s.Apply(Update{ID: "jd-tgw", State: ptr("x")}); !errors.Is(err, ErrNoName) {
		t.Fatalf("a new front with no name: %v", err)
	}
	f, err := s.Apply(Update{ID: "jd-tgw", Name: "JD TGW 迁移", State: ptr("EC2 VXLAN 方案被否"), Next: ptr("改迁移路线图"),
		Waiting: ptr("客户"), Sessions: []string{"dashboard:direct:a:general"}, Decision: "先把 prefix 的资源迁回 JDOS VPC"})
	if err != nil || f.Name != "JD TGW 迁移" || f.Waiting != "客户" || len(f.Decisions) != 1 || !f.Created.Equal(now) {
		t.Fatalf("made %+v, %v", f, err)
	}

	now = now.Add(time.Hour)
	f, err = s.Apply(Update{ID: "jd-tgw", Waiting: ptr(""), Sessions: []string{"dashboard:direct:b:general", "dashboard:direct:a:general"}, Decision: "终态放 8468"})
	if err != nil || f.State != "EC2 VXLAN 方案被否" || f.Next != "改迁移路线图" || f.Waiting != "" {
		t.Fatalf("a field not named changed, or waiting not cleared: %+v, %v", f, err)
	}
	if !slices.Equal(f.Sessions, []string{"dashboard:direct:b:general", "dashboard:direct:a:general"}) ||
		f.Decisions[0].Text != "终态放 8468" || len(f.Decisions) != 2 || !f.Updated.Equal(now) {
		t.Fatalf("sessions %q, decisions %+v", f.Sessions, f.Decisions)
	}

	if _, err := s.Apply(Update{ID: "blog", Name: "trn2 blog"}); err != nil {
		t.Fatal(err)
	}
	again := New(path, func() time.Time { return now })
	if open := again.Open(); len(open) != 2 || open[0].ID != "blog" || open[1].ID != "jd-tgw" || len(open[1].Decisions) != 2 {
		t.Fatalf("after a reload: %+v", open)
	}

	if f, err := again.Apply(Update{ID: "blog", Close: true}); err != nil || f.Open() {
		t.Fatalf("close: %+v, %v", f, err)
	}
	if open := again.Open(); len(open) != 1 || open[0].ID != "jd-tgw" {
		t.Fatalf("open after a close: %+v", open)
	}
	if f, err := again.Apply(Update{ID: "blog", State: ptr("同事又提了意见")}); err != nil || !f.Open() || f.Name != "trn2 blog" {
		t.Fatalf("reopen: %+v, %v", f, err)
	}
}

// What a front keeps is bounded, the closed ones go after their retention,
// and the open ones are capped.
func TestBounds(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	s := New("", func() time.Time { return now })
	for i := range MaxSessions + 3 {
		if _, err := s.Apply(Update{ID: "a", Name: "甲", Sessions: []string{fmt.Sprintf("k%d", i)}, Decision: fmt.Sprintf("d%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	f := s.Open()[0]
	if len(f.Sessions) != MaxSessions || f.Sessions[0] != fmt.Sprintf("k%d", MaxSessions+2) || len(f.Decisions) != MaxSessions+3 {
		t.Fatalf("sessions %q, %d decisions", f.Sessions, len(f.Decisions))
	}
	for i := 1; i < MaxOpen; i++ {
		if _, err := s.Apply(Update{ID: fmt.Sprintf("f%d", i), Name: "乙"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Apply(Update{ID: "one-more", Name: "丙"}); !errors.Is(err, ErrTooMany) {
		t.Fatalf("past the cap: %v", err)
	}
	if _, err := s.Apply(Update{ID: "f1", Close: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(Update{ID: "one-more", Name: "丙"}); err != nil {
		t.Fatalf("after a close: %v", err)
	}
	if _, err := s.Apply(Update{ID: "f1", Next: ptr("x")}); !errors.Is(err, ErrTooMany) {
		t.Fatalf("reopening past the cap: %v", err)
	}
	now = now.Add(closedRetention + time.Hour)
	if _, err := s.Apply(Update{ID: "a", Next: ptr("y")}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	kept := slices.ContainsFunc(s.fronts, func(f Front) bool { return f.ID == "f1" })
	s.mu.Unlock()
	if kept {
		t.Error("a front closed past its retention is still kept")
	}
}

func TestValidate(t *testing.T) {
	long := strings.Repeat("字", TextRunes+1)
	for _, tc := range []struct {
		name string
		u    Update
		ok   bool
	}{
		{"plain", Update{ID: " jd-tgw ", Name: " JD ", State: ptr(" x ")}, true},
		{"bad id", Update{ID: "JD TGW", Name: "x"}, false},
		{"no id", Update{Name: "x"}, false},
		{"long name", Update{ID: "a", Name: strings.Repeat("字", NameRunes+1)}, false},
		{"long state", Update{ID: "a", State: &long}, false},
		{"long decision", Update{ID: "a", Decision: strings.Repeat("字", DecisionRunes+1)}, false},
		{"empty session", Update{ID: "a", Sessions: []string{" "}}, false},
		{"too many sessions", Update{ID: "a", Sessions: make([]string, MaxSessions+1)}, false},
	} {
		err := tc.u.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
		if tc.name == "plain" && (tc.u.ID != "jd-tgw" || tc.u.Name != "JD" || *tc.u.State != "x") {
			t.Errorf("not trimmed: %+v", tc.u)
		}
	}
}
