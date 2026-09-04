package agentcore

// TerminalState is the three-way completion classification of a sandbox run.
// It feeds the double-run containment rules, so when in doubt: FailedTransport.
type TerminalState string

const (
	// Success: the stream-json result event arrived with is_error=false.
	Success TerminalState = "success"
	// FailedClean: failure attested by the stream itself (result with
	// is_error=true, or exit frame without a result). Replay is reasonably safe.
	FailedClean TerminalState = "failed-clean"
	// FailedTransport: stream broke without attestation; the microVM may still
	// run. NOT safe to replay until StopRuntimeSession has been confirmed.
	FailedTransport TerminalState = "failed-transport"
)

// classifier folds the envelope stream into a TerminalState. Only stream
// content is evidence (the AWS call returning rc=0 is not):
//
//	result(is_error=false) seen                    → Success
//	result(is_error=true) seen                     → FailedClean
//	no result, exit frame seen, stream EOF clean   → FailedClean
//	no result, stream error or no exit frame       → FailedTransport
type classifier struct {
	sawResult     bool
	resultIsError bool
	sawExit       bool
	exitCode      int
}

func (c *classifier) observe(env *Envelope) {
	switch env.Kind {
	case KindCLI:
		if isRes, isErr := isResultLine(env.Line); isRes {
			c.sawResult = true
			c.resultIsError = isErr
		}
	case KindExit:
		c.sawExit = true
		c.exitCode = env.Code
	case KindBoot, KindKeepalive, KindMeta:
		// No terminal-state signal; KindMeta listed so nobody wires it in.
	}
}

// terminal returns the final state given the stream-read error. Any
// streamErr means FailedTransport: a result seen before a mid-stream break
// does not prove the CLI's teardown completed.
func (c *classifier) terminal(streamErr error) TerminalState {
	if streamErr != nil {
		return FailedTransport
	}
	if c.sawResult {
		if c.resultIsError {
			return FailedClean
		}
		return Success
	}
	if c.sawExit {
		return FailedClean
	}
	// Clean EOF but no result and no exit frame (e.g. idle-timeout burn
	// mid-job): the microVM's fate is unknown.
	return FailedTransport
}
