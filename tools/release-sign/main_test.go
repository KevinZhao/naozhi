package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/selfupdate"
)

type harness struct {
	vars           map[string]string
	stdout, stderr bytes.Buffer
	terminal       bool
	trust          []ed25519.PublicKey
}

func (h *harness) run(args ...string) int {
	h.stdout.Reset()
	h.stderr.Reset()
	return run(args, env{
		getenv:           func(k string) string { return h.vars[k] },
		stdout:           &h.stdout,
		stderr:           &h.stderr,
		rand:             rand.Reader,
		stdoutIsTerminal: h.terminal,
		trust:            h.trust,
	})
}

// newKey returns a throwaway test key as the base64 seed sign reads.
func newKey(t *testing.T) (ed25519.PublicKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, base64.StdEncoding.EncodeToString(priv.Seed())
}

// release writes a checksums.txt and returns its path and the .sig path next to it.
func release(t *testing.T) (in, sig string) {
	t.Helper()
	dir := t.TempDir()
	in = filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(in, []byte(strings.Repeat("a", 64)+"  naozhi-linux-amd64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return in, filepath.Join(dir, "checksums.txt.sig")
}

func TestKeygenSignVerify_RoundTrip(t *testing.T) {
	t.Parallel()
	h := &harness{}
	if code := h.run("keygen"); code != 0 {
		t.Fatalf("keygen = %d: %s", code, h.stderr.String())
	}
	seed := h.stdout.String()
	if raw, err := base64.StdEncoding.DecodeString(seed); err != nil || len(raw) != ed25519.SeedSize {
		t.Fatalf("stdout must be exactly a base64 %d-byte seed, got %q (%v)", ed25519.SeedSize, seed, err)
	}
	pubB64 := strings.TrimSpace(h.stderr.String()[strings.LastIndex(h.stderr.String(), " ")+1:])
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		t.Fatalf("stderr must end with the base64 public key, got %q", h.stderr.String())
	}
	if strings.Contains(h.stderr.String(), seed) {
		t.Fatal("the seed must appear on stdout only")
	}

	in, sigPath := release(t)
	h.vars = map[string]string{signingKeyEnv: seed + "\n"}
	h.trust = []ed25519.PublicKey{pub}
	if code := h.run("sign", "-in", in, "-out", sigPath); code != 0 {
		t.Fatalf("sign = %d: %s%s", code, h.stdout.String(), h.stderr.String())
	}
	sig, err := os.ReadFile(sigPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.ContainsAny(sig, "\r\n") {
		t.Fatalf("signature must have no newline, got %q", sig)
	}
	raw, err := base64.StdEncoding.DecodeString(string(sig))
	if err != nil || len(raw) != ed25519.SignatureSize {
		t.Fatalf("signature must be base64 of %d bytes, got %q", ed25519.SignatureSize, sig)
	}
	payload, _ := os.ReadFile(in)
	if _, err := selfupdate.VerifyChecksumsSignature(payload, sig, h.trust); err != nil {
		t.Fatalf("the client-side check must accept the signature: %v", err)
	}
	if code := h.run("verify", "-in", in, "-sig", sigPath); code != 0 {
		t.Fatalf("verify = %d: %s", code, h.stdout.String())
	}
}

func TestKeygen_RefusesTerminal(t *testing.T) {
	t.Parallel()
	h := &harness{terminal: true}
	if code := h.run("keygen"); code != 2 {
		t.Fatalf("keygen to a terminal = %d, want 2", code)
	}
	if h.stdout.Len() != 0 {
		t.Fatalf("no seed may reach a terminal, got %q", h.stdout.String())
	}
}

func TestSign_NoKeyEmptyTrust_SkipsWithWarning(t *testing.T) {
	t.Parallel()
	h := &harness{}
	in, sigPath := release(t)
	if code := h.run("sign", "-in", in, "-out", sigPath); code != 0 {
		t.Fatalf("sign = %d, want 0 while nothing is embedded", code)
	}
	if !strings.Contains(h.stdout.String(), "::warning::") {
		t.Fatalf("want a workflow warning, got %q", h.stdout.String())
	}
	if _, err := os.Stat(sigPath); !os.IsNotExist(err) {
		t.Fatalf("no signature file may be written, stat err = %v", err)
	}
}

func TestSign_NoKeyWithTrust_Fails(t *testing.T) {
	t.Parallel()
	pub, _ := newKey(t)
	h := &harness{trust: []ed25519.PublicKey{pub}}
	in, sigPath := release(t)
	if code := h.run("sign", "-in", in, "-out", sigPath); code != 1 {
		t.Fatalf("sign without a key while clients embed one = %d, want 1", code)
	}
	if _, err := os.Stat(sigPath); !os.IsNotExist(err) {
		t.Fatalf("no signature file may be written, stat err = %v", err)
	}
}

func TestSign_KeyOutsideTrustSet_Fails(t *testing.T) {
	t.Parallel()
	embedded, _ := newKey(t)
	_, other := newKey(t)
	h := &harness{trust: []ed25519.PublicKey{embedded}, vars: map[string]string{signingKeyEnv: other}}
	in, sigPath := release(t)
	if code := h.run("sign", "-in", in, "-out", sigPath); code != 1 {
		t.Fatalf("sign with an untrusted key = %d, want 1", code)
	}
	if _, err := os.Stat(sigPath); !os.IsNotExist(err) {
		t.Fatalf("no signature file may be written, stat err = %v", err)
	}
}

func TestSign_MalformedSeed_FailsWithoutEchoingIt(t *testing.T) {
	t.Parallel()
	for _, seed := range []string{"not base64 at all", base64.StdEncoding.EncodeToString([]byte("too short"))} {
		h := &harness{vars: map[string]string{signingKeyEnv: seed}}
		in, sigPath := release(t)
		if code := h.run("sign", "-in", in, "-out", sigPath); code != 1 {
			t.Fatalf("sign with seed %q = %d, want 1", seed, code)
		}
		if out := h.stdout.String() + h.stderr.String(); strings.Contains(out, seed) {
			t.Fatalf("the secret must not be echoed, got %q", out)
		}
	}
}

func TestVerify_Refusals(t *testing.T) {
	t.Parallel()
	pub, seed := newKey(t)
	otherPub, otherSeed := newKey(t)
	trust := []ed25519.PublicKey{pub}

	signed := func(t *testing.T, seed string, trust []ed25519.PublicKey) (string, string) {
		in, sigPath := release(t)
		h := &harness{vars: map[string]string{signingKeyEnv: seed}, trust: trust}
		if code := h.run("sign", "-in", in, "-out", sigPath); code != 0 {
			t.Fatalf("sign = %d: %s", code, h.stdout.String())
		}
		return in, sigPath
	}
	cases := []struct {
		name  string
		setup func(t *testing.T) (in, sig string)
	}{
		{"missing signature", func(t *testing.T) (string, string) { return release(t) }},
		{"tampered payload", func(t *testing.T) (string, string) {
			in, sigPath := signed(t, seed, trust)
			if err := os.WriteFile(in, []byte(strings.Repeat("b", 64)+"  naozhi-linux-amd64\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return in, sigPath
		}},
		{"untrusted key", func(t *testing.T) (string, string) {
			return signed(t, otherSeed, []ed25519.PublicKey{otherPub})
		}},
		{"malformed signature", func(t *testing.T) (string, string) {
			in, sigPath := release(t)
			if err := os.WriteFile(sigPath, []byte("!!"), 0o644); err != nil {
				t.Fatal(err)
			}
			return in, sigPath
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in, sigPath := c.setup(t)
			h := &harness{trust: trust}
			if code := h.run("verify", "-in", in, "-sig", sigPath); code != 1 {
				t.Fatalf("verify = %d, want 1: %s", code, h.stdout.String())
			}
			if !strings.Contains(h.stdout.String(), "::error::") {
				t.Fatalf("want a workflow error, got %q", h.stdout.String())
			}
		})
	}
}

func TestVerify_EmptyTrust_Passes(t *testing.T) {
	t.Parallel()
	_, seed := newKey(t)
	h := &harness{}
	in, sigPath := release(t)
	if code := h.run("verify", "-in", in, "-sig", sigPath); code != 0 {
		t.Fatalf("verify with no key and no signature = %d, want 0", code)
	}
	h.vars = map[string]string{signingKeyEnv: seed}
	if code := h.run("sign", "-in", in, "-out", sigPath); code != 0 {
		t.Fatalf("sign = %d", code)
	}
	h.vars = nil
	if code := h.run("verify", "-in", in, "-sig", sigPath); code != 0 {
		t.Fatalf("verify with no key and a signature = %d, want 0", code)
	}
	if !strings.Contains(h.stdout.String(), "::warning::") {
		t.Fatalf("an unchecked signature must be flagged, got %q", h.stdout.String())
	}
}

func TestVerify_EmptyTrust_UnreadableSigFails(t *testing.T) {
	t.Parallel()
	h := &harness{}
	in, _ := release(t)
	dir := t.TempDir()
	if code := h.run("verify", "-in", in, "-sig", dir); code != 1 {
		t.Fatalf("verify with an unreadable signature path = %d, want 1", code)
	}
}

func TestRun_Usage(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{nil, {"bogus"}, {"sign"}, {"sign", "-in", "x"}, {"verify", "-sig", "x"}, {"sign", "-in", "x", "-out", "y", "extra"}} {
		h := &harness{}
		if code := h.run(args...); code != 2 {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
	}
}
