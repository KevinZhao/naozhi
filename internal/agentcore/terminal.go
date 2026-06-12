package agentcore

// TerminalState is the three-way completion classification of a sandbox run
// (RFC §6.1). The three states differ in side-effect certainty and replay
// safety — they are the input to the §6.2 double-run containment rules, so
// classification must be conservative: when in doubt, FailedTransport.
type TerminalState string

const (
	// Success: the CLI's stream-json result event arrived with
	// is_error=false. The job completed as intended.
	Success TerminalState = "success"
	// FailedClean: the run failed but the failure is *attested by the
	// stream itself* — a result event with is_error=true, or a clean
	// stream end (exit frame seen) without any result. Side effects most
	// likely did not happen or are incomplete; replay is reasonably safe.
	FailedClean TerminalState = "failed-clean"
	// FailedTransport: the stream broke without a terminal attestation —
	// connection reset, context cancellation, missing exit frame. The
	// microVM may still be running and may have produced side effects.
	// NOT safe to replay until StopRuntimeSession has been confirmed
	// (RFC §6.2 rule 1).
	FailedTransport TerminalState = "failed-transport"
)

// classifier folds the envelope stream into a TerminalState. Feed every
// decoded envelope in order, then call Terminal with the stream error.
//
// Classification table (validation report §4: the AWS call returning rc=0
// is NOT evidence of completion — only stream content is):
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
		// Diagnostics, liveness, and the execution receipt (image/memory)
		// carry no terminal-state signal — only result/exit decide the
		// §6.1 classification. KindMeta is listed explicitly so a future
		// editor does not mistake its absence for an oversight and wire it
		// into sawResult/sawExit.
	}
}

// terminal returns the final state given the stream-read error (nil for a
// clean EOF). streamErr non-nil always means FailedTransport: a result
// event seen before a mid-stream break does not prove the CLI's post-result
// teardown completed, and §6.2 demands conservatism.
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
		// CLI died (or handler failed) without producing a result, but the
		// bootstrap attested the death and the stream closed cleanly.
		return FailedClean
	}
	// Clean EOF but no result and no exit frame: the stream was cut at a
	// layer that still produced a well-formed HTTP end (e.g. idle-timeout
	// burn mid-job, observed in validation V8 first attempt). The microVM's
	// fate is unknown.
	return FailedTransport
}
