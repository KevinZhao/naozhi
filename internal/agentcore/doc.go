// Package agentcore is the control-plane client for the AgentCore cloud
// sandbox placement (docs/rfc/agentcore-cloud-sandbox.md). It invokes an AWS
// Bedrock AgentCore Runtime running the naozhi bootstrap handler
// (spike/agentcore/bootstrap) and holds the streaming SSE envelope response;
// the microVM never reaches back.
//
// It owns exactly three concerns: payload construction, holding/decoding the
// event stream (SSE envelope → raw claude stream-json lines, keepalives
// filtered), and terminal-state classification. Full stream-json parsing
// stays with cli.Protocol.
package agentcore
