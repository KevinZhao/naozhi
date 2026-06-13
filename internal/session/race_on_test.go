//go:build race

package session

// raceEnabled is true when the test binary was built with -race. Used by
// allocation-sensitive tests to skip testing.AllocsPerRun assertions, which the
// race detector makes non-deterministic. See store_marshal_pool_test.go.
const raceEnabled = true
