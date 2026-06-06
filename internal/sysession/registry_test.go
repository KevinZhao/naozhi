package sysession

import "testing"

func TestValidateDaemonName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"valid kebab", "auto-titler", false},
		{"valid single segment", "ab", false},
		// Regex is ^[a-z][a-z0-9-]{1,30}$ — 1 leading letter + 1..30
		// trailing chars = total length 2..31.
		{"valid 31 chars", "a234567890123456789012345678901", false},
		{"empty rejected", "", true},
		{"single char rejected (too short)", "a", true},
		{"32 chars rejected", "a2345678901234567890123456789012", true},
		{"leading digit rejected", "1auto", true},
		{"leading hyphen rejected", "-auto", true},
		{"underscore rejected", "auto_titler", true},
		{"uppercase rejected", "AutoTitler", true},
		{"colon rejected", "auto:titler", true},
		{"dot rejected", "auto.titler", true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := validateDaemonName(c.in)
			if (err != nil) != c.wantErr {
				t.Errorf("validateDaemonName(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			}
		})
	}
}

// TestBuiltinDaemonNameConstants pins the daemon-name constants to the
// registry factory entries AND to each constructed daemon's Name(), so a
// rename of one site without the others fails compilation/this test
// rather than silently drifting (#1634). The config-translation wiring in
// cmd/naozhi references the same constants, closing the third site.
func TestBuiltinDaemonNameConstants(t *testing.T) {
	registryTestMu.Lock()
	defer registryTestMu.Unlock()

	// Constant -> the factory expected to carry it.
	byName := make(map[string]builtinDaemonFactory, len(builtinDaemons))
	for _, f := range builtinDaemons {
		byName[f.Name] = f
	}
	for _, want := range []string{DaemonAutoTitler, DaemonAttachmentGC} {
		if _, ok := byName[want]; !ok {
			t.Errorf("builtinDaemons missing factory for constant %q", want)
		}
	}

	deps := DaemonDeps{Router: wrapRouter(newFakeRouter()), Runner: &capturingRunner{}}
	for _, f := range builtinDaemons {
		d, err := f.Build(deps)
		if err != nil {
			t.Fatalf("Build(%q): %v", f.Name, err)
		}
		if got := d.Name(); got != f.Name {
			t.Errorf("daemon Name() = %q, factory Name = %q (constant drift)", got, f.Name)
		}
	}
}

// TestValidateBuiltinDaemonNames verifies the compiled-in registry is
// healthy at process start.  This test failing means a future
// contributor added a daemon with a malformed or duplicate name; fix
// the registry, not this test.
//
// NOT t.Parallel():  Manager tests use withRegistry() to swap
// builtinDaemons in/out under registryTestMu.  We acquire that mutex
// here so the race detector sees a happens-before edge with those swaps.
func TestValidateBuiltinDaemonNames(t *testing.T) {
	registryTestMu.Lock()
	defer registryTestMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("builtinDaemons must satisfy validateBuiltinDaemonNames; panic: %v", r)
		}
	}()
	validateBuiltinDaemonNames()
}

// TestBuiltinDaemonsSliceLiteralInvariant pins the R244-ARCH-18 (#1055)
// decision documented above builtinDaemons in registry.go: sysession keeps a
// static, eagerly-populated slice literal rather than cli/history's
// blank-import + init()-driven registry.  It asserts the registry is fully
// populated at package init (not via late init() registration), every entry
// carries a non-nil Build factory, and every Name passes validateDaemonName.
//
// If a future change (e.g. the R244-ARCH-4/#1058 unified-Registry[T] work in
// internal/wireup) deliberately moves sysession onto an init()-based or
// blank-import registry, update this test alongside the registry.go anchor
// comment — do not silently relax it.
//
// NOT t.Parallel(): like the sibling registry pins, Manager tests swap
// builtinDaemons via withRegistry() under registryTestMu, so we take that
// mutex to give the race detector a happens-before edge with those swaps.
func TestBuiltinDaemonsSliceLiteralInvariant(t *testing.T) {
	registryTestMu.Lock()
	defer registryTestMu.Unlock()

	if len(builtinDaemons) == 0 {
		t.Fatal("builtinDaemons is empty: expected an eagerly-populated static slice literal, not init()-driven late registration")
	}
	for i, f := range builtinDaemons {
		if f.Build == nil {
			t.Errorf("builtinDaemons[%d] (%q): Build factory is nil", i, f.Name)
		}
		if err := validateDaemonName(f.Name); err != nil {
			t.Errorf("builtinDaemons[%d] (%q): invalid name: %v", i, f.Name, err)
		}
	}
}
