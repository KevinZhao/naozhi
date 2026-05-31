package cli

// Runnable pins for the R219-ARCH-6 (#668) additive facet split: the
// ProtocolCore (seven protocol-agnostic methods) and
// ProtocolPassthroughExt (four stream-json-flavoured methods) interfaces
// are each a strict subset of the Protocol god-interface, so every
// concrete Protocol implementation (production *ClaudeProtocol /
// *ACPProtocol) also satisfies both facets and a narrow consumer can
// switch to the smaller seam without an adapter.
//
// The `var _ ProtocolCore = (Protocol)(nil)` / `var _
// ProtocolPassthroughExt = (Protocol)(nil)` pins in protocol.go cover the
// language-level invariant; this file pins the production implementations
// as runnable `go test` targets so a method rename/removal that breaks the
// partition shows up in CI output, not only at first build of a dependent
// package. Mirrors internal/session/process_lifecycle_test.go (#430).

import "testing"

func TestProtocolFacets_SubsetOfClaudeProtocol(t *testing.T) {
	t.Parallel()
	var p Protocol = &ClaudeProtocol{}
	// Narrow to each facet; if the split ever drops a method, these
	// assignments stop compiling.
	var core ProtocolCore = p
	var ext ProtocolPassthroughExt = p
	// Reach through the narrow seams so the variables are load-bearing.
	if got := core.Name(); got == "" {
		t.Error("ClaudeProtocol.Name() via ProtocolCore should be non-empty")
	}
	if !ext.SupportsReplay() {
		t.Error("ClaudeProtocol via ProtocolPassthroughExt should SupportsReplay")
	}
}

func TestProtocolFacets_SubsetOfACPProtocol(t *testing.T) {
	t.Parallel()
	var p Protocol = &ACPProtocol{}
	var core ProtocolCore = p
	var ext ProtocolPassthroughExt = p
	if got := core.Name(); got != "acp" {
		t.Errorf("ACPProtocol.Name() via ProtocolCore = %q, want acp", got)
	}
	// ACP degrades the passthrough surface (the leak R219-ARCH-6 flags):
	// SupportsReplay/SupportsPriority are false. Assert that contract so
	// the facet's intent (stream-json-only methods that noop on ACP)
	// stays documented and enforced.
	if ext.SupportsReplay() {
		t.Error("ACPProtocol via ProtocolPassthroughExt should not SupportsReplay")
	}
}
