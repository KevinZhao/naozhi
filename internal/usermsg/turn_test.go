package usermsg

import (
	"strconv"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

func TestForTurnResult(t *testing.T) {
	t.Parallel()
	rpc := func(backend string, code int, msg string) *clievent.SendResult {
		return &clievent.SendResult{
			Text: "[" + backend + "] acp rpc error " + msg, SubType: "error", IsError: true,
			BackendError: &clievent.BackendError{Backend: backend, Code: code, Message: msg},
		}
	}
	tests := []struct {
		name      string
		r         *clievent.SendResult
		wantClass string // "" = no notice
		wantSubs  []string
	}{
		{"nil", nil, "", nil},
		{"answer", &clievent.SendResult{Text: "done", SubType: "success"}, "", nil},
		{"empty tool-only turn", &clievent.SendResult{SubType: "success"}, "", nil},
		{"is_error with text is not a notice", &clievent.SendResult{Text: "Prompt is too long", SubType: "success", IsError: true}, "", nil},
		{"max turns", &clievent.SendResult{SubType: "error_max_turns", IsError: true}, "max_turns", []string{"最大执行步数", "继续"}},
		{"max budget", &clievent.SendResult{SubType: "error_max_budget_usd", IsError: true}, "max_budget", []string{"费用上限"}},
		{"during execution", &clievent.SendResult{SubType: "error_during_execution", IsError: true}, "turn_failed", []string{"中途出错", "/new"}},
		{"own abort", &clievent.SendResult{SubType: "error_during_execution", IsError: true, Aborted: true}, "", nil},
		{"abort does not mute max turns", &clievent.SendResult{SubType: "error_max_turns", IsError: true, Aborted: true}, "max_turns", nil},
		{"unknown error subtype", &clievent.SendResult{SubType: "error_something_new"}, "turn_failed", nil},
		{"bare is_error", &clievent.SendResult{IsError: true}, "turn_failed", nil},
		{"acp refusal", &clievent.SendResult{SubType: "refusal"}, "refused", []string{"拒绝"}},
		{"acp max_tokens", &clievent.SendResult{SubType: "max_tokens"}, "truncated", []string{"输出上限"}},
		{"acp max_turn_requests", &clievent.SendResult{SubType: "max_turn_requests"}, "max_turns", []string{"最大执行步数"}},
		{"acp tool_use_failure", &clievent.SendResult{SubType: "tool_use_failure"}, "turn_failed", nil},
		{"acp max_tokens with text is delivered", &clievent.SendResult{Text: "partial", SubType: "max_tokens"}, "", nil},
		{"acp cancelled", &clievent.SendResult{SubType: "cancelled", Aborted: true}, "", nil},
		{"rpc overload by wording", rpc("kiro", -32000, "model overloaded"), "backend_overloaded", []string{"kiro 服务当前负载较高"}},
		{"codex -32001", rpc("codex", -32001, "busy"), "backend_overloaded", []string{"codex "}},
		{"rpc rate limit", rpc("kiro", -32000, "Too Many Requests"), "backend_rate_limited", []string{"调用过于频繁"}},
		{"rpc auth", rpc("kiro", -32000, "Authentication required"), "backend_auth", []string{"认证失败"}},
		{"rpc invalid params", rpc("kiro", -32602, "Invalid params: model overloaded"), "backend_invalid_request", []string{"请求格式无效"}},
		{"rpc other", rpc("kiro", -32603, "internal error"), "backend_rejected", []string{"kiro 未能完成本轮请求"}},
		{"kiro -32603 with data detail", rpc("kiro", -32603, "Internal error: The model you've selected is temporarily unavailable."), "backend_overloaded", []string{"kiro "}},
		{"codex failed turn", &clievent.SendResult{
			Text: "stream disconnected", SubType: "error", IsError: true,
			BackendError: &clievent.BackendError{Backend: "codex", Message: "stream disconnected"},
		}, "backend_rejected", []string{"codex "}},
		{"unnamed backend", &clievent.SendResult{IsError: true, BackendError: &clievent.BackendError{Code: -32001}}, "backend_overloaded", []string{"后端服务当前负载较高"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, class := ForTurnResult(tt.r)
			if class != tt.wantClass {
				t.Fatalf("ForTurnResult class = %q (text %q), want %q", class, got, tt.wantClass)
			}
			if tt.wantClass == "" {
				if got != "" {
					t.Errorf("ForTurnResult = %q, want no notice", got)
				}
				return
			}
			if got == "" || got == genericRetryHint {
				t.Errorf("ForTurnResult = %q, want a dedicated notice", got)
			}
			for _, want := range tt.wantSubs {
				if !strings.Contains(got, want) {
					t.Errorf("ForTurnResult = %q, missing %q", got, want)
				}
			}
			if be := tt.r.BackendError; be != nil {
				if strings.Contains(got, "rpc error") || (be.Message != "" && strings.Contains(got, be.Message)) {
					t.Errorf("ForTurnResult = %q leaks the backend's raw error", got)
				}
				if be.Code != 0 && strings.Contains(got, strconv.Itoa(-be.Code)) {
					t.Errorf("ForTurnResult = %q leaks the RPC code %d", got, be.Code)
				}
			}
		})
	}
}

// TestTurnClass_EveryTurnCode: each turn Code has a class label, so the
// metric and log never record "" for a notice that went out.
func TestTurnClass_EveryTurnCode(t *testing.T) {
	t.Parallel()
	for c := CodeTurnFailed; c < codeEnd; c++ {
		if turnClass[c] == "" {
			t.Errorf("Code %d has no turnClass label", int(c))
		}
	}
}
