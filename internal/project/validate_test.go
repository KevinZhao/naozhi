package project

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  ProjectConfig
		ok   bool
	}{
		{"empty passes", ProjectConfig{}, true},
		{"plain model passes", ProjectConfig{PlannerModel: "claude-sonnet-4-6"}, true},
		{"model with slash allowed", ProjectConfig{PlannerModel: "anthropic/claude-opus"}, true},
		{"model with dot/colon allowed", ProjectConfig{PlannerModel: "eu-west-1.bedrock:claude"}, true},
		{"prompt with tab allowed", ProjectConfig{PlannerPrompt: "line1\n"[:0] + "\tindent"}, true},

		{"prompt over cap rejected", ProjectConfig{PlannerPrompt: strings.Repeat("a", 8*1024+1)}, false},
		{"prompt with NUL rejected", ProjectConfig{PlannerPrompt: "foo\x00bar"}, false},
		{"prompt with LF rejected", ProjectConfig{PlannerPrompt: "foo\nbar"}, false},
		{"prompt with CR rejected", ProjectConfig{PlannerPrompt: "foo\rbar"}, false},
		{"prompt with DEL rejected", ProjectConfig{PlannerPrompt: "foo\x7fbar"}, false},
		{"prompt with ESC rejected", ProjectConfig{PlannerPrompt: "foo\x1bbar"}, false},

		{"model over cap rejected", ProjectConfig{PlannerModel: strings.Repeat("a", 257)}, false},
		{"model with space rejected", ProjectConfig{PlannerModel: "claude --dangerously"}, false},
		{"model with leading dash rejected", ProjectConfig{PlannerModel: "-rm-rf"}, false},
		{"model with newline rejected", ProjectConfig{PlannerModel: "claude\nfoo"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateConfig(c.cfg)
			if c.ok && err != nil {
				t.Errorf("unexpected err: %v", err)
			}
			if !c.ok {
				if err == nil {
					t.Errorf("expected error, got nil")
				} else if !errors.Is(err, ErrInvalidConfig) {
					t.Errorf("err = %v, want wrap of ErrInvalidConfig", err)
				}
			}
		})
	}
}
