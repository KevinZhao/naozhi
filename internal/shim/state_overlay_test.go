package shim

// state_overlay_test.go — on-disk contract for State.SpawnOverlay (#2494).
//
// The field is three-valued on read (nil = legacy writer, non-nil zero = known
// empty, populated = overrides in effect) and additive (no SchemaVersion
// bump), so a rolled-back binary keeps reconnecting to shims spawned by this
// one. Both properties are pinned here.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestState_SpawnOverlay_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overlay.json")
	want := State{
		ShimPID: 1, Socket: "/tmp/s.sock", AuthToken: "dA==", Key: "k",
		CLIArgs: []string{"-p", "--model", "sonnet"},
		SpawnOverlay: &SpawnOverlay{
			Model: "sonnet", Effort: "max", ExtraArgs: []string{"--x", "1"}, AccessProfile: "work",
		},
	}
	if err := WriteStateFile(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SpawnOverlay == nil {
		t.Fatal("SpawnOverlay lost in round-trip")
	}
	if got.SpawnOverlay.Model != "sonnet" || got.SpawnOverlay.Effort != "max" ||
		got.SpawnOverlay.AccessProfile != "work" ||
		!slices.Equal(got.SpawnOverlay.ExtraArgs, []string{"--x", "1"}) {
		t.Errorf("SpawnOverlay = %+v, want %+v", *got.SpawnOverlay, *want.SpawnOverlay)
	}
	// Additive contract: writing the new field must NOT advance the advisory
	// schema marker, or a downgraded reader refuses to reconnect (state.go
	// "Versioning contract").
	if got.SchemaVersion > 1 {
		t.Errorf("SchemaVersion = %d; SpawnOverlay is additive and must not bump it", got.SchemaVersion)
	}
}

// TestState_SpawnOverlay_KnownEmptyIsNonNil is the semantic the drift check
// depends on: a writer that KNOWS the overlay is empty must persist `{}`, so a
// reader can distinguish it from a legacy file that omitted the key.
func TestState_SpawnOverlay_KnownEmptyIsNonNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty_overlay.json")
	st := State{ShimPID: 1, Socket: "/tmp/s.sock", AuthToken: "dA==", Key: "k", SpawnOverlay: &SpawnOverlay{}}
	if err := WriteStateFile(path, st); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"spawn_overlay": {}`) {
		t.Errorf("empty overlay must be written as {} (known-empty), got:\n%s", raw)
	}
	got, err := ReadStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SpawnOverlay == nil {
		t.Fatal("known-empty overlay decoded to nil — indistinguishable from a legacy file")
	}
	if ov := got.SpawnOverlay; ov.Model != "" || ov.Effort != "" || ov.AccessProfile != "" || len(ov.ExtraArgs) != 0 {
		t.Errorf("known-empty overlay decoded populated: %+v", *ov)
	}
}

// TestState_SpawnOverlay_LegacyFileDecodesNil: a state file written by a
// pre-#2494 shim has no spawn_overlay key and must decode to nil (not to an
// empty struct), so the reader can take its documented fallback path.
func TestState_SpawnOverlay_LegacyFileDecodesNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.json")
	payload := `{"version":1,"shim_pid":1,"cli_pid":0,"socket":"/tmp/s.sock","auth_token":"dA==","key":"k","session_id":"","workspace":"","cli_args":["-p","--model","opusplan"],"cli_alive":true,"started_at":"","buffer_count":0}`
	if err := os.WriteFile(path, []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStateFile(path)
	if err != nil {
		t.Fatalf("legacy state must still load: %v", err)
	}
	if got.SpawnOverlay != nil {
		t.Errorf("legacy file decoded a non-nil overlay %+v; must be nil", *got.SpawnOverlay)
	}
	if !slices.Equal(got.CLIArgs, []string{"-p", "--model", "opusplan"}) {
		t.Errorf("CLIArgs = %v", got.CLIArgs)
	}
}

// TestState_SpawnOverlay_OmittedWhenNil: a nil overlay (legacy caller through
// StartShim) writes no key at all — byte-compatible with the pre-#2494 layout.
func TestState_SpawnOverlay_OmittedWhenNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nil_overlay.json")
	if err := WriteStateFile(path, State{ShimPID: 1, Socket: "/tmp/s.sock", AuthToken: "dA==", Key: "k"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "spawn_overlay") {
		t.Errorf("nil overlay must be omitted from JSON, got:\n%s", raw)
	}
}

func TestSpawnOverlay_EncodeDecode(t *testing.T) {
	t.Run("nil encodes to empty string and decodes back to nil", func(t *testing.T) {
		s, err := EncodeSpawnOverlay(nil)
		if err != nil || s != "" {
			t.Fatalf("EncodeSpawnOverlay(nil) = %q, %v", s, err)
		}
		ov, err := DecodeSpawnOverlay("")
		if err != nil || ov != nil {
			t.Fatalf("DecodeSpawnOverlay(\"\") = %+v, %v; want nil, nil", ov, err)
		}
	})
	t.Run("known-empty survives as non-nil", func(t *testing.T) {
		s, err := EncodeSpawnOverlay(&SpawnOverlay{})
		if err != nil || s != "{}" {
			t.Fatalf("EncodeSpawnOverlay(&{}) = %q, %v; want {}", s, err)
		}
		ov, err := DecodeSpawnOverlay(s)
		if err != nil || ov == nil {
			t.Fatalf("DecodeSpawnOverlay(%q) = %v, %v; want non-nil", s, ov, err)
		}
	})
	t.Run("populated round-trips", func(t *testing.T) {
		in := &SpawnOverlay{Model: "sonnet", Effort: "max", ExtraArgs: []string{"--append-system-prompt", "multi word\nprompt"}, AccessProfile: "work"}
		s, err := EncodeSpawnOverlay(in)
		if err != nil {
			t.Fatal(err)
		}
		out, err := DecodeSpawnOverlay(s)
		if err != nil {
			t.Fatal(err)
		}
		if out.Model != in.Model || out.Effort != in.Effort || out.AccessProfile != in.AccessProfile ||
			!slices.Equal(out.ExtraArgs, in.ExtraArgs) {
			t.Errorf("round-trip = %+v, want %+v", *out, *in)
		}
	})
	t.Run("malformed input is an error, not a silent nil", func(t *testing.T) {
		if ov, err := DecodeSpawnOverlay("{not json"); err == nil {
			t.Fatalf("expected error, got %+v", ov)
		}
	})
}
