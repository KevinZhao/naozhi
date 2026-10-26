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
