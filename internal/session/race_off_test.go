//go:build !race

package session

// raceEnabled is false in non-race builds; allocation-sensitive tests run their
// AllocsPerRun assertions. See store_marshal_pool_test.go.
const raceEnabled = false
