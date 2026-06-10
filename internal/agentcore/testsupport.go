package agentcore

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
)

// RuntimeAPI re-exports the SDK seam so consumer packages (wireup) can
// fake the AWS edge in their own tests without real credentials. Mirrors
// the runtimeAPI interface exactly; production code must keep using New.
type RuntimeAPI interface {
	InvokeAgentRuntime(ctx context.Context, params *bedrockagentcore.InvokeAgentRuntimeInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error)
	StopRuntimeSession(ctx context.Context, params *bedrockagentcore.StopRuntimeSessionInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.StopRuntimeSessionOutput, error)
}

// NewWithAPIForTest builds a Client over a fake API. Test seam for
// consumer packages; never wire production traffic through it.
func NewWithAPIForTest(api RuntimeAPI, cfg Config) *Client {
	return newWithAPI(api, cfg)
}

// NewRunIDForTest returns a valid fresh run id for consumer-package tests.
func NewRunIDForTest() string { return NewRunID(time.Now()) }
