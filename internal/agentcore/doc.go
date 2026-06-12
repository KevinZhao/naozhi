// Package agentcore is the control-plane client for the AgentCore cloud
// sandbox placement (docs/rfc/agentcore-cloud-sandbox.md §4.2/§4.3).
//
// It speaks to an AWS Bedrock AgentCore Runtime whose container runs the
// naozhi bootstrap handler (spike/agentcore/bootstrap): InvokeAgentRuntime
// carries the injection payload, and the streaming HTTP response carries
// the bootstrap's SSE envelope back (decision A1-a: naozhi holds the
// stream; the microVM never needs to reach back).
//
// The package owns exactly three concerns:
//
//   - payload construction (what gets injected into the microVM),
//   - holding and decoding the event stream (SSE envelope → raw claude
//     stream-json lines, keepalives filtered),
//   - terminal-state classification (success / failed-clean /
//     failed-transport, RFC §6.1 — the input to the §6.2 double-run
//     containment rules).
//
// It deliberately does NOT parse claude stream-json beyond the minimal
// result/exit envelope fields needed for classification — full event
// parsing stays with cli.Protocol (the placement axis never forks the
// protocol, RFC v1.3 repositioning).
package agentcore
