package digest

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/planner/activity"
)

var cst = time.FixedZone("CST", 8*3600)

func at(h, m int) time.Time { return time.Date(2026, 11, 8, h, m, 0, 0, cst) }

func fullFacts() Facts {
	return Facts{
		Key:         "task:abc123",
		Title:       "修 index.json 的 last_active",
		Workspace:   "naozhi",
		Dir:         "/w/naozhi",
		Purpose:     "task",
		Health:      health.Idle,
		HealthSince: at(16, 59),
		LastOutput:  at(16, 58),
		Goal:        "把 last_active 改成按事件刷新",
		Report: &Report{State: "done", Summary: "已修好并开了 PR。CI 绿", At: at(16, 40),
			Text: "做了什么：改了 health_sources.go\n\n## 没做什么\n命令：go test ./... 退出码 0"},
		Replies: []Reply{
			{At: at(16, 30), Text: "第一轮"},
			{At: at(16, 58), Text: strings.Repeat("长", ReplyRunes+5)},
		},
		Outputs: []activity.Output{
			{Time: at(16, 50), Activity: activity.Activity{Type: activity.TypeOutput, Path: "docs/x.md", Dir: "/w/naozhi"}},
			{Time: at(16, 45), Activity: activity.Activity{Type: activity.TypePRCreate, URL: "https://github.com/o/r/pull/1", State: "opened"}},
		},
		Questions: []string{"要不要合并？"},
		Updated:   at(17, 0),
	}
}

// TestRender pins the digest's layout: every section, the caps, and how an
// empty one reads (the Planner reads these as they are).
func TestRender(t *testing.T) {
	for _, c := range []struct {
		name string
		f    Facts
	}{
		{"full", fullFacts()},
		{"empty", Facts{Key: "dashboard:direct:x:general", Updated: at(9, 0)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Render(c.f)
			p := filepath.Join("testdata", c.name+".golden.md")
			if os.Getenv("UPDATE_GOLDEN") != "" {
				if err := os.WriteFile(p, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("Render = \n%s\nwant\n%s", got, want)
			}
			key, updated, _, ok := parseHeader(got)
			if !ok || key != c.f.Key || !updated.Equal(c.f.Updated) {
				t.Errorf("parseHeader = %q %v %v", key, updated, ok)
			}
		})
	}
}

func TestGoal(t *testing.T) {
	for in, want := range map[string]string{
		"修好 X\n顺便看 Y\n\n细节：…":            "修好 X 顺便看 Y",
		"  一段  ":                         "一段",
		"":                               "",
		strings.Repeat("字", GoalRunes+1): strings.Repeat("字", GoalRunes) + "...",
	} {
		if got := Goal(in); got != want {
			t.Errorf("Goal(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFirstSentence(t *testing.T) {
	for in, want := range map[string]string{
		"已修好。CI 绿":        "已修好。",
		"升到 v0.7.0. 然后发版": "升到 v0.7.0.",
		"Done! next":      "Done!",
		"没有句号":            "没有句号",
		"读 a/b.md 后":      "读 a/b.md 后",
	} {
		if got := FirstSentence(in); got != want {
			t.Errorf("FirstSentence(%q) = %q, want %q", in, got, want)
		}
	}
}
