package project

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateConfig(t *testing.T) {
	t.Parallel()
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

		// R184-SEC-M1: ChatBindings must reject colon / NUL / oversize so
		// the bindingIndex key "platform:chatType:chatID" stays unambiguous.
		{"bindings plain pass", ProjectConfig{ChatBindings: []ChatBinding{{Platform: "feishu", ChatType: "group", ChatID: "oc_abc"}}}, true},
		{"bindings colon in platform rejected", ProjectConfig{ChatBindings: []ChatBinding{{Platform: "fei:shu", ChatID: "oc"}}}, false},
		{"bindings colon in chat_type rejected", ProjectConfig{ChatBindings: []ChatBinding{{Platform: "feishu", ChatType: "group:evil", ChatID: "oc"}}}, false},
		{"bindings colon in chat_id rejected", ProjectConfig{ChatBindings: []ChatBinding{{Platform: "feishu", ChatID: "oc:evil"}}}, false},
		{"bindings NUL in chat_id rejected", ProjectConfig{ChatBindings: []ChatBinding{{Platform: "feishu", ChatID: "oc\x00"}}}, false},
		{"bindings oversized chat_id rejected", ProjectConfig{ChatBindings: []ChatBinding{{Platform: "feishu", ChatID: strings.Repeat("x", 257)}}}, false},
		{"bindings oversized platform rejected", ProjectConfig{ChatBindings: []ChatBinding{{Platform: strings.Repeat("x", 65), ChatID: "oc"}}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
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

// R181-SEC-P2-2: ValidateProjectName is shared between the dashboard
// HTTP path and the reverse-RPC update_config / restart_planner worker.
// Locks the policy so a future relaxation has to update both trust
// boundaries together.
func TestValidateProjectName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		ok    bool
	}{
		{"simple ascii passes", "my-project", true},
		{"dot passes", "naozhi.v2", true},
		{"underscore passes", "proj_a", true},
		{"cjk passes", "项目", true},

		{"empty rejected", "", false},
		{"oversize rejected", strings.Repeat("a", MaxProjectNameBytes+1), false},
		{"NUL rejected", "foo\x00bar", false},
		{"LF rejected", "foo\nbar", false},
		{"CR rejected", "foo\rbar", false},
		{"DEL rejected", "foo\x7fbar", false},
		{"C1 NEL rejected", "foo\u0085bar", false},
		{"bidi override rejected", "foo\u202ebar", false},
		{"bidi isolate rejected", "foo\u2068bar", false},
		{"LS rejected", "foo\u2028bar", false},
		{"invalid utf-8 rejected", string([]byte{'f', 'o', 0xC3, 'o'}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateProjectName(c.input)
			if c.ok && err != nil {
				t.Errorf("unexpected err: %v", err)
			}
			if !c.ok && err == nil {
				t.Errorf("expected error for %q, got nil", c.input)
			}
		})
	}
}
