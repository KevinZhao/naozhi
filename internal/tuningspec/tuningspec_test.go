package tuningspec

import "testing"

// TestValidateModel_ContextWindowSuffix pins that the claude CLI's
// context-window suffix ("…[1m]") is a valid model id end to end: the CLI
// echoes it in its init frame, the observed-model manifest offers it back,
// and the dashboard override must accept the pick. The leading-char gate
// (no `-`, no `[`) stays strict — that is the flag-injection guard.
func TestValidateModel_ContextWindowSuffix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"fable_1m", "us.anthropic.claude-fable-5-1[1m]", false},
		{"opus_1m", "us.anthropic.claude-opus-5[1m]", false},
		{"alias_1m", "opus[1m]", false},
		{"plain", "us.anthropic.claude-fable-5-1", false},
		{"leading_bracket_rejected", "[1m]", true},
		{"leading_dash_rejected", "-inject", true},
		{"space_rejected", "opus [1m]", true},
		{"brace_rejected", "opus{1m}", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateModel("test", tc.value)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateModel(%q) = %v, wantErr=%v", tc.value, err, tc.wantErr)
			}
		})
	}
}
