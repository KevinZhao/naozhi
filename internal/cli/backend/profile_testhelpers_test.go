package backend

import "fmt"

// mustGet is the test-only "panic if missing" lookup helper. It used to
// live on the production API surface as MustGet, but DEADCODE-11 found
// no production caller — backends are loaded at startup and the
// registry is constant after init, so the panic-on-missing property is
// only useful in tests where the fixture has just been primed.
//
// Lives in a _test.go file so the production binary does not carry the
// extra symbol; tests in the same package see it via lowercase.
func mustGet(id string) Profile {
	p, ok := Get(id)
	if !ok {
		panic(fmt.Sprintf("backend: unknown id %q", id))
	}
	return p
}
