package naozhisettings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// unmarshalTop is a test helper: parse a settings doc into a top-level map.
func unmarshalTop(t *testing.T, doc []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(doc, &m); err != nil {
		t.Fatalf("produced doc is not a JSON object: %v\n%s", err, doc)
	}
	return m
}

func TestBootstrap_StripsHooksAndEnv_PreservesRest(t *testing.T) {
	local := []byte(`{
		"hooks": {"PostToolUse": [{"matcher": "*", "hooks": []}]},
		"env": {"ANTHROPIC_API_KEY": "sk-secret", "FOO": "bar"},
		"model": "opus",
		"permissions": {"allow": ["Bash(ls)"]},
		"mcpServers": {"x": {"command": "y"}}
	}`)
	doc, ok, err := Bootstrap(local)
	if err != nil {
		t.Fatalf("Bootstrap error: %v", err)
	}
	if !ok {
		t.Fatalf("Bootstrap ok=false for well-formed input")
	}
	top := unmarshalTop(t, doc)
	if _, has := top["hooks"]; has {
		t.Error("hooks must be stripped")
	}
	if _, has := top["env"]; has {
		t.Error("env must be stripped (auth goes via access-profile overlay)")
	}
	// Non-stripped keys preserved verbatim.
	for _, k := range []string{"model", "permissions", "mcpServers"} {
		if _, has := top[k]; !has {
			t.Errorf("key %q must be preserved", k)
		}
	}
	// The secret from the env block must not survive anywhere in the output.
	if containsSecret := string(doc); contains(containsSecret, "sk-secret") {
		t.Errorf("secret token leaked into naozhi settings: %s", containsSecret)
	}
}

func TestBootstrap_PreservesNestedValueByteForByte(t *testing.T) {
	local := []byte(`{"permissions":{"allow":["Bash(ls)","Read"],"deny":[]}}`)
	doc, ok, err := Bootstrap(local)
	if err != nil || !ok {
		t.Fatalf("Bootstrap ok=%v err=%v", ok, err)
	}
	top := unmarshalTop(t, doc)
	var got, want map[string]any
	if err := json.Unmarshal(top["permissions"], &got); err != nil {
		t.Fatalf("permissions not preserved as object: %v", err)
	}
	_ = json.Unmarshal([]byte(`{"allow":["Bash(ls)","Read"],"deny":[]}`), &want)
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Errorf("permissions value drifted: got %s want %s", gb, wb)
	}
}

func TestBootstrap_FailSoftOnMissingOrCorrupt(t *testing.T) {
	for name, in := range map[string][]byte{
		"nil":        nil,
		"empty":      {},
		"corrupt":    []byte(`{not json`),
		"non-object": []byte(`["a","b"]`),
	} {
		doc, ok, err := Bootstrap(in)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
		if ok {
			t.Errorf("%s: ok should be false for missing/corrupt input", name)
		}
		// Must still yield a valid, empty JSON object.
		top := unmarshalTop(t, doc)
		if len(top) != 0 {
			t.Errorf("%s: fail-soft doc should be empty object, got %s", name, doc)
		}
	}
}

func TestEnsureBootstrapped_DoesNotOverwriteExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "naozhi-settings.json")
	// Pre-existing naozhi file with a tuned value.
	tuned := []byte(`{"model":"tuned-by-operator"}`)
	if err := os.WriteFile(path, tuned, 0o600); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(dir, "local.json")
	if err := os.WriteFile(local, []byte(`{"model":"from-local"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	existed, seeded, err := EnsureBootstrapped(path, local)
	if err != nil {
		t.Fatalf("EnsureBootstrapped: %v", err)
	}
	if !existed {
		t.Error("existed should be true")
	}
	if seeded {
		t.Error("must NOT reseed an existing file")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(tuned) {
		t.Errorf("existing file was overwritten: %s", got)
	}
}

func TestEnsureBootstrapped_SeedsWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "naozhi-settings.json")
	local := filepath.Join(dir, "local.json")
	if err := os.WriteFile(local, []byte(`{"hooks":{"x":1},"model":"opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	existed, seeded, err := EnsureBootstrapped(path, local)
	if err != nil {
		t.Fatalf("EnsureBootstrapped: %v", err)
	}
	if existed {
		t.Error("existed should be false")
	}
	if !seeded {
		t.Error("seeded should be true from a well-formed local file")
	}
	top := unmarshalTop(t, mustRead(t, path))
	if _, has := top["hooks"]; has {
		t.Error("seeded file must have hooks stripped")
	}
	if _, has := top["model"]; !has {
		t.Error("seeded file must keep model")
	}
	// 0600 perms.
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("naozhi settings perms = %o, want 600", fi.Mode().Perm())
	}
}

func TestEnsureBootstrapped_MissingLocalIsNonFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "naozhi-settings.json")
	local := filepath.Join(dir, "does-not-exist.json")
	existed, seeded, err := EnsureBootstrapped(path, local)
	if err != nil {
		t.Fatalf("missing local should be non-fatal, got %v", err)
	}
	if existed || seeded {
		t.Errorf("existed=%v seeded=%v, want both false", existed, seeded)
	}
	top := unmarshalTop(t, mustRead(t, path))
	if len(top) != 0 {
		t.Errorf("expected empty seed doc, got %s", mustRead(t, path))
	}
}

func TestReBootstrap_Overwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "naozhi-settings.json")
	if err := os.WriteFile(path, []byte(`{"model":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(dir, "local.json")
	if err := os.WriteFile(local, []byte(`{"model":"new","env":{"K":"v"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	seeded, err := ReBootstrap(path, local)
	if err != nil {
		t.Fatalf("ReBootstrap: %v", err)
	}
	if !seeded {
		t.Error("seeded should be true")
	}
	top := unmarshalTop(t, mustRead(t, path))
	var model string
	_ = json.Unmarshal(top["model"], &model)
	if model != "new" {
		t.Errorf("model = %q, want new (overwrite)", model)
	}
	if _, has := top["env"]; has {
		t.Error("env must be stripped on re-bootstrap too")
	}
}

// TestEnsureBootstrapped_CreatesMissingParentDir locks F1/F4: a first-run host
// whose data dir does not yet exist must still get a written naozhi file — the
// package owns the MkdirAll, callers must not have to pre-create the dir.
func TestEnsureBootstrapped_CreatesMissingParentDir(t *testing.T) {
	dir := t.TempDir()
	// A nested dir that does NOT exist yet (mirrors a fresh data root).
	path := filepath.Join(dir, "state", "sub", "naozhi-settings.json")
	local := filepath.Join(dir, "local.json")
	if err := os.WriteFile(local, []byte(`{"model":"opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	existed, _, err := EnsureBootstrapped(path, local)
	if err != nil {
		t.Fatalf("EnsureBootstrapped must create missing parent dir, got err=%v", err)
	}
	if existed {
		t.Error("existed should be false")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("file not written into freshly-created dir: %v", statErr)
	}
}

// TestBootstrap_PreservesTopLevelKeyOrder locks F3: keys are re-emitted in the
// original document order (not alphabetised), so the RFC §5.3 diff stays a
// low-noise view of real differences.
func TestBootstrap_PreservesTopLevelKeyOrder(t *testing.T) {
	// Deliberately non-alphabetical order.
	local := []byte(`{"zulu":1,"alpha":2,"env":{"X":"y"},"mike":3,"hooks":{"a":1},"bravo":4}`)
	doc, ok, err := Bootstrap(local)
	if err != nil || !ok {
		t.Fatalf("Bootstrap ok=%v err=%v", ok, err)
	}
	// Extract top-level keys in emission order via a streaming decoder.
	keys, _, perr := parseTopLevelOrdered(doc)
	if perr != nil {
		t.Fatalf("produced doc not parseable: %v\n%s", perr, doc)
	}
	want := []string{"zulu", "alpha", "mike", "bravo"} // env + hooks stripped, rest in order
	if len(keys) != len(want) {
		t.Fatalf("key set = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("key order[%d] = %q, want %q (full: %v)", i, keys[i], want[i], keys)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
