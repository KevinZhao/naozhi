package cli

// protocol_acp_models_test.go — model-manifest capture semantics (F5/F12).
// docs/rfc/dashboard-model-effort-control.md §4.2 / §5 协议 ACP row.

import "testing"

func TestACPProtocol_StoreModels(t *testing.T) {
	t.Parallel()
	p := &ACPProtocol{}

	if got := p.AvailableModels(); got != nil {
		t.Fatalf("pre-init manifest = %v, want nil", got)
	}

	// session/new result shape (F5).
	p.captureModels([]byte(`{"sessionId":"s1","models":{"currentModelId":"claude-fable-5",
		"availableModels":[
			{"modelId":"auto","name":"auto","description":"picks per task"},
			{"modelId":"claude-haiku-4.5","name":"claude-haiku-4.5"},
			{"modelId":"","name":"broken entry — must be dropped"}
		]}}`))
	models := p.AvailableModels()
	if len(models) != 2 {
		t.Fatalf("models = %v, want 2 entries (empty id dropped)", models)
	}
	if models[0].ID != "auto" || models[0].Description != "picks per task" {
		t.Errorf("models[0] = %+v", models[0])
	}

	// A later result WITHOUT models must not wipe the good manifest
	// (transiently silent agent).
	p.captureModels([]byte(`{"sessionId":"s2"}`))
	if len(p.AvailableModels()) != 2 {
		t.Error("empty envelope wiped a previously captured manifest")
	}

	// Unparseable results are ignored — the manifest is an enhancement,
	// never a reason to fail Init (§5 "解析失败不阻塞 Init").
	p.captureModels([]byte(`{invalid`))
	if len(p.AvailableModels()) != 2 {
		t.Error("parse failure wiped the manifest")
	}

	// A fresh result replaces (kiro upgrade re-reports on next spawn, §6 R4).
	p.captureModels([]byte(`{"models":{"availableModels":[{"modelId":"new-model"}]}}`))
	if got := p.AvailableModels(); len(got) != 1 || got[0].ID != "new-model" {
		t.Errorf("refresh failed: %v", got)
	}
}
