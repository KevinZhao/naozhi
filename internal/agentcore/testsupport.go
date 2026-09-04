package agentcore

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
)

// RuntimeAPI re-exports the SDK seam so consumer packages can fake the AWS
// edge in their tests. Mirrors runtimeAPI exactly; production code uses New.
type RuntimeAPI interface {
	InvokeAgentRuntime(ctx context.Context, params *bedrockagentcore.InvokeAgentRuntimeInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error)
	StopRuntimeSession(ctx context.Context, params *bedrockagentcore.StopRuntimeSessionInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.StopRuntimeSessionOutput, error)
}

// NewWithAPIForTest builds a Client over a fake API. Never wire production
// traffic through it.
func NewWithAPIForTest(api RuntimeAPI, cfg Config) *Client {
	return newWithAPI(api, cfg)
}
