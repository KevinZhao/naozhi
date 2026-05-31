package backend

import (
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// withCleanRegistry runs fn against an empty registry and restores the
// pre-test state afterwards so tests cannot leak state into one another.
// Tests that exercise registration order or duplicate-panic semantics need
// this; pure read-only tests against RegisterDefaults can also use it for
// isolation.
//
// defaultsOnce is reset to a fresh sync.Once alongside the registry so a
// test that calls EnsureDefaults() inside the clean block actually
// re-bootstraps. Without this reset the block would inherit a fired
// defaultsOnce from an earlier test (or production init), making
// EnsureDefaults a silent no-op against the empty registry — the test
// would then observe All() len 0 purely as a function of run order
// (#895: test isolation must not depend on which test fires the Once first).
func withCleanRegistry(t *testing.T, fn func()) {
	t.Helper()
	registryMu.Lock()
	savedRegistry := registry
	savedOrder := nextOrder
	savedOnce := defaultsOnce
	registry = map[string]registryEntry{}
	nextOrder = 0
	defaultsOnce = sync.Once{}
	registryMu.Unlock()

	t.Cleanup(func() {
		registryMu.Lock()
		registry = savedRegistry
		nextOrder = savedOrder
		defaultsOnce = savedOnce
		registryMu.Unlock()
	})

	fn()
}

func sampleProfile(id string) Profile {
	return Profile{
		ID:            id,
		DisplayName:   id + "-display",
		DefaultBinary: id + "-bin",
		DefaultTag:    id,
		NewProtocol: func(_ ProtocolDeps) cli.Protocol {
			return &cli.ClaudeProtocol{}
		},
		DetectInProc: func(_ string) bool { return false },
	}
}

func TestRegister_PanicOnDuplicate(t *testing.T) {
	withCleanRegistry(t, func() {
		Register(sampleProfile("alpha"))

		defer func() {
			r := recover()
			if r == nil {
				t.Fatalf("Register should panic on duplicate id, got no panic")
			}
			msg, ok := r.(string)
			if !ok {
				t.Fatalf("expected string panic, got %T (%v)", r, r)
			}
			if !strings.Contains(msg, "duplicate registration") {
				t.Errorf("panic message %q missing %q", msg, "duplicate registration")
			}
			if !strings.Contains(msg, "alpha") {
				t.Errorf("panic message %q missing offending id %q", msg, "alpha")
			}
		}()

		Register(sampleProfile("alpha"))
	})
}

func TestGet_MissingReturnsFalse(t *testing.T) {
	withCleanRegistry(t, func() {
		Register(sampleProfile("alpha"))

		got, ok := Get("nope")
		if ok {
			t.Fatalf("Get(\"nope\") = ok=true; want false")
		}
		if got.ID != "" {
			t.Errorf("Get on missing id returned non-zero Profile: %+v", got)
		}

		got, ok = Get("alpha")
		if !ok {
			t.Fatalf("Get(\"alpha\") = ok=false; want true")
		}
		if got.ID != "alpha" {
			t.Errorf("Get returned profile.ID = %q; want %q", got.ID, "alpha")
		}
	})
}

func TestMustGet_PanicOnMissing(t *testing.T) {
	withCleanRegistry(t, func() {
		Register(sampleProfile("alpha"))

		// Hit path: should not panic.
		got := mustGet("alpha")
		if got.ID != "alpha" {
			t.Errorf("MustGet returned ID %q; want %q", got.ID, "alpha")
		}

		// Miss path: must panic with "unknown id".
		defer func() {
			r := recover()
			if r == nil {
				t.Fatalf("MustGet on missing id should panic")
			}
			msg, ok := r.(string)
			if !ok {
				t.Fatalf("expected string panic, got %T (%v)", r, r)
			}
			if !strings.Contains(msg, "unknown id") {
				t.Errorf("panic message %q missing %q", msg, "unknown id")
			}
		}()
		mustGet("missing")
	})
}

func TestAll_SortedByRegistrationOrder(t *testing.T) {
	withCleanRegistry(t, func() {
		// Intentionally register out of alphabetical order so that
		// alphabetical sorting and registration-order sorting differ.
		ids := []string{"zeta", "alpha", "mu", "beta"}
		for _, id := range ids {
			Register(sampleProfile(id))
		}

		all := All()
		if len(all) != len(ids) {
			t.Fatalf("All() returned %d profiles; want %d", len(all), len(ids))
		}
		for i, want := range ids {
			if all[i].ID != want {
				t.Errorf("All()[%d].ID = %q; want %q (full order: %v)",
					i, all[i].ID, want, ids)
			}
		}

		// Mutating the returned slice must not affect subsequent calls.
		all[0] = Profile{ID: "tampered"}
		all2 := All()
		if all2[0].ID != "zeta" {
			t.Errorf("All() returned shared slice; got %q after caller mutation", all2[0].ID)
		}
	})
}

func TestRegisterDefaults_RegistersClaudeAndKiro(t *testing.T) {
	withCleanRegistry(t, func() {
		RegisterDefaults()

		all := All()
		if len(all) != 2 {
			t.Fatalf("RegisterDefaults: All() len = %d; want 2", len(all))
		}

		// Registration order is claude then kiro per the function body.
		if all[0].ID != "claude" {
			t.Errorf("first default profile = %q; want %q", all[0].ID, "claude")
		}
		if all[1].ID != "kiro" {
			t.Errorf("second default profile = %q; want %q", all[1].ID, "kiro")
		}

		claude, ok := Get("claude")
		if !ok {
			t.Fatal("Get(\"claude\") missing after RegisterDefaults")
		}
		if claude.DisplayName != "claude-code" {
			t.Errorf("claude DisplayName = %q; want %q", claude.DisplayName, "claude-code")
		}
		if claude.DefaultBinary != "claude" {
			t.Errorf("claude DefaultBinary = %q; want %q", claude.DefaultBinary, "claude")
		}
		if claude.DefaultTag != "cc" {
			t.Errorf("claude DefaultTag = %q; want %q", claude.DefaultTag, "cc")
		}
		if len(claude.RequiredNodeCaps) != 0 {
			t.Errorf("claude RequiredNodeCaps = %v; want empty", claude.RequiredNodeCaps)
		}

		kiro, ok := Get("kiro")
		if !ok {
			t.Fatal("Get(\"kiro\") missing after RegisterDefaults")
		}
		if kiro.DisplayName != "kiro" {
			t.Errorf("kiro DisplayName = %q; want %q", kiro.DisplayName, "kiro")
		}
		if kiro.DefaultBinary != "kiro-cli" {
			t.Errorf("kiro DefaultBinary = %q; want %q", kiro.DefaultBinary, "kiro-cli")
		}
		if kiro.DefaultTag != "kiro" {
			t.Errorf("kiro DefaultTag = %q; want %q", kiro.DefaultTag, "kiro")
		}
		if len(kiro.RequiredNodeCaps) != 1 || kiro.RequiredNodeCaps[0] != "acp" {
			t.Errorf("kiro RequiredNodeCaps = %v; want [\"acp\"]", kiro.RequiredNodeCaps)
		}
	})
}

// TestEnsureDefaults_IdempotentAndConcurrent pins the contract added by
// PR #122 follow-up: EnsureDefaults must (a) register defaults exactly
// once even under N concurrent callers, and (b) be safe to call after
// RegisterDefaults already ran. Earlier recover()-based bootstrap could
// leak partial registrations when two goroutines raced through
// RegisterDefaults.
func TestEnsureDefaults_IdempotentAndConcurrent(t *testing.T) {
	withCleanRegistry(t, func() {
		const N = 50
		var wg sync.WaitGroup
		wg.Add(N)
		for i := 0; i < N; i++ {
			go func() {
				defer wg.Done()
				EnsureDefaults()
			}()
		}
		wg.Wait()

		all := All()
		if len(all) != 2 {
			t.Fatalf("after %d concurrent EnsureDefaults: All() len = %d; want 2", N, len(all))
		}
		if _, ok := Get("claude"); !ok {
			t.Error("claude missing after concurrent EnsureDefaults")
		}
		if _, ok := Get("kiro"); !ok {
			t.Error("kiro missing after concurrent EnsureDefaults")
		}

		// Subsequent calls must be no-ops (no panic, no extra registrations).
		EnsureDefaults()
		EnsureDefaults()
		if got := len(All()); got != 2 {
			t.Errorf("repeat EnsureDefaults registered extra: All() len = %d; want 2", got)
		}
	})
}

// TestEnsureDefaults_CleanRegistryReBootstraps guards the test-isolation
// fix in #895: a clean-registry block must re-bootstrap via EnsureDefaults
// regardless of whether defaultsOnce already fired in production init or an
// earlier test. We fire EnsureDefaults() OUTSIDE the clean block first to
// trip the package-level Once, then assert that inside withCleanRegistry the
// defaults re-register. Pre-fix (defaultsOnce not reset) this observed
// All() len 0 — a run-order-dependent failure.
func TestEnsureDefaults_CleanRegistryReBootstraps(t *testing.T) {
	// Trip the global Once outside the clean block (idempotent / safe).
	EnsureDefaults()

	withCleanRegistry(t, func() {
		if got := len(All()); got != 0 {
			t.Fatalf("clean registry should start empty; All() len = %d", got)
		}
		EnsureDefaults()
		if got := len(All()); got != 2 {
			t.Fatalf("EnsureDefaults inside clean registry did not re-bootstrap: All() len = %d; want 2 (defaultsOnce reset missing?)", got)
		}
		if _, ok := Get("claude"); !ok {
			t.Error("claude missing after re-bootstrap")
		}
		if _, ok := Get("kiro"); !ok {
			t.Error("kiro missing after re-bootstrap")
		}
	})
}

func TestProfile_NewProtocol_Claude(t *testing.T) {
	withCleanRegistry(t, func() {
		RegisterDefaults()

		p := mustGet("claude")
		if p.NewProtocol == nil {
			t.Fatal("claude profile NewProtocol is nil")
		}

		// direct-user-settings PR1: ProtocolDeps is empty (the settings
		// override plumbing was removed; cc loads ~/.claude/settings.json
		// directly via --setting-sources user). NewProtocol must still build
		// a usable ClaudeProtocol from the empty deps.
		proto := p.NewProtocol(ProtocolDeps{})

		if _, ok := proto.(*cli.ClaudeProtocol); !ok {
			t.Fatalf("claude NewProtocol returned %T; want *cli.ClaudeProtocol", proto)
		}
		if proto.Name() != "stream-json" {
			t.Errorf("claude protocol Name() = %q; want %q", proto.Name(), "stream-json")
		}
	})
}

func TestProfile_NewProtocol_Kiro(t *testing.T) {
	withCleanRegistry(t, func() {
		RegisterDefaults()

		p := mustGet("kiro")
		if p.NewProtocol == nil {
			t.Fatal("kiro profile NewProtocol is nil")
		}

		// Kiro ignores ProtocolDeps but accept any value without panicking.
		proto := p.NewProtocol(ProtocolDeps{})

		if _, ok := proto.(*cli.ACPProtocol); !ok {
			t.Fatalf("kiro NewProtocol returned %T; want *cli.ACPProtocol", proto)
		}
		if proto.Name() != "acp" {
			t.Errorf("kiro protocol Name() = %q; want %q", proto.Name(), "acp")
		}
	})
}

func TestProfile_DetectInProc(t *testing.T) {
	withCleanRegistry(t, func() {
		RegisterDefaults()
	})

	// The defaults are now restored to the saved registry. We need to
	// re-register inside a clean scope to keep this case hermetic.
	withCleanRegistry(t, func() {
		RegisterDefaults()

		claude := mustGet("claude")
		kiro := mustGet("kiro")

		cases := []struct {
			name       string
			cmdline    string
			wantClaude bool
			wantKiro   bool
		}{
			{
				name:       "plain claude binary",
				cmdline:    "/usr/local/bin/claude --output-format stream-json",
				wantClaude: true,
				wantKiro:   false,
			},
			{
				name:       "plain kiro binary",
				cmdline:    "/usr/local/bin/kiro-cli acp",
				wantClaude: false,
				wantKiro:   true,
			},
			{
				name:       "neither cli",
				cmdline:    "/usr/bin/python3 /opt/foo/server.py",
				wantClaude: false,
				wantKiro:   false,
			},
			{
				name:       "boundary: kiro-claude-fake.exe contains both substrings",
				cmdline:    "/opt/imposter/claude-kiro-fake.exe --help",
				wantClaude: false, // kiro substring excludes claude predicate
				wantKiro:   true,
			},
			{
				name:       "claude with kiro in the workspace path is mis-attributed to kiro by design",
				cmdline:    "/usr/local/bin/claude --cwd /home/me/kiro-workspace",
				wantClaude: false, // documented limitation: cmdline contains "kiro"
				wantKiro:   true,
			},
			{
				name:       "empty cmdline",
				cmdline:    "",
				wantClaude: false,
				wantKiro:   false,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := claude.DetectInProc(tc.cmdline); got != tc.wantClaude {
					t.Errorf("claude.DetectInProc(%q) = %v; want %v", tc.cmdline, got, tc.wantClaude)
				}
				if got := kiro.DetectInProc(tc.cmdline); got != tc.wantKiro {
					t.Errorf("kiro.DetectInProc(%q) = %v; want %v", tc.cmdline, got, tc.wantKiro)
				}
			})
		}
	})
}
