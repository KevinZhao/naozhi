package agentlink_test

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session/agentlink"
)

// TestSubagentLinkerSatisfiesAgentLinker pins that the production producer
// *cli.SubagentLinker still satisfies the composite AgentLinker after the
// facet split (R248-ARCH-4 #402 part c). A compile-time assignment is the
// strongest guard against signature drift.
func TestSubagentLinkerSatisfiesAgentLinker(t *testing.T) {
	var _ agentlink.AgentLinker = cli.NewSubagentLinker()
}

// TestFacetsComposeIntoAgentLinker pins that the three single-responsibility
// facets compose to exactly the AgentLinker method set: any type satisfying
// all three facets must satisfy AgentLinker, and vice versa. This guards the
// embedding contract so a future method added to AgentLinker is forced into
// one of the facets rather than silently widening the composite.
func TestFacetsComposeIntoAgentLinker(t *testing.T) {
	var l agentlink.AgentLinker = cli.NewSubagentLinker()

	// Each facet is independently satisfiable from the composite value.
	var _ agentlink.Notifier = l
	var _ agentlink.Resolver = l
	var _ agentlink.PathProvider = l

	// And a value satisfying all three facets is an AgentLinker.
	type allFacets interface {
		agentlink.Notifier
		agentlink.Resolver
		agentlink.PathProvider
	}
	var af allFacets = cli.NewSubagentLinker()
	var _ agentlink.AgentLinker = af
}
