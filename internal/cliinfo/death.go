package cliinfo

// Death reason labels. Kept as exported constants so session/router callers
// can match without relying on stringly-typed literals that drift.
const (
	DeathReasonCLIExited           = "cli_exited"
	DeathReasonShimEOF             = "shim_eof"
	DeathReasonShimReadErr         = "shim_read_error"
	DeathReasonShimOversizeThenEOF = "shim_oversize_then_eof"
	DeathReasonShimOversizeThenErr = "shim_oversize_then_read_error"
	DeathReasonReadLoopPanic       = "readloop_panic"
	DeathReasonKilled              = "killed"
	DeathReasonNoOutputTimeout     = "no_output_timeout"
	DeathReasonTotalTimeout        = "total_timeout"
)

// A CLI that exits on its own records DeathReasonCLIExited, or the code prefix
// plus its non-zero exit code (-1 when a signal killed it), or the signal
// prefix plus the signal name a shim reported with a zero code. contractjs
// emits both prefixes so the dashboard can name the code or signal.
const (
	DeathReasonCLIExitedCodePrefix   = DeathReasonCLIExited + "_code_"
	DeathReasonCLIExitedSignalPrefix = DeathReasonCLIExited + "_signal_"
)

// AllDeathReasons lists every DeathReason* constant's value, so contractjs can
// enumerate the wire vocabulary instead of restating it as JS string literals.
// death_test.go keeps it in step with the const block above.
func AllDeathReasons() []string {
	return []string{
		DeathReasonCLIExited,
		DeathReasonShimEOF,
		DeathReasonShimReadErr,
		DeathReasonShimOversizeThenEOF,
		DeathReasonShimOversizeThenErr,
		DeathReasonReadLoopPanic,
		DeathReasonKilled,
		DeathReasonNoOutputTimeout,
		DeathReasonTotalTimeout,
	}
}
