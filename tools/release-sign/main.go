// Command release-sign signs a release's checksums.txt with the ed25519
// release key and checks the signature against the trust set embedded in
// internal/selfupdate at the tag being released (#1738). Clients already
// deployed verify with the set they shipped with, which this cannot see.
// Run from the repo root:
//
//	go run ./tools/release-sign keygen | gh secret set NAOZHI_RELEASE_SIGNING_KEY --env release
//	go run ./tools/release-sign sign -in dist/checksums.txt -out dist/checksums.txt.sig
//	go run ./tools/release-sign verify -in dist/checksums.txt -sig dist/checksums.txt.sig
//
// keygen writes the base64 seed to stdout (refusing a terminal, so the seed
// lands only in the pipe) and the base64 public key to stderr. sign reads the
// seed from NAOZHI_RELEASE_SIGNING_KEY and writes a base64 signature with no
// trailing newline.
//
// While the embedded trust set is empty, clients check no signature, so sign
// without a key and verify without a signature pass with a warning. Once a key
// is embedded both fail instead: a release its own trust set would refuse
// must not publish.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/naozhi/naozhi/internal/selfupdate"
)

const signingKeyEnv = "NAOZHI_RELEASE_SIGNING_KEY"

// env is everything a subcommand touches outside its flags.
type env struct {
	getenv           func(string) string
	stdout, stderr   io.Writer
	rand             io.Reader
	stdoutIsTerminal bool
	trust            []ed25519.PublicKey
}

func main() {
	os.Exit(run(os.Args[1:], env{
		getenv:           os.Getenv,
		stdout:           os.Stdout,
		stderr:           os.Stderr,
		rand:             rand.Reader,
		stdoutIsTerminal: isTerminal(os.Stdout),
		trust:            selfupdate.TrustedSigKeys(),
	}))
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func run(args []string, e env) int {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, "usage: release-sign keygen | sign -in FILE -out SIG | verify -in FILE -sig SIG")
		return 2
	}
	switch args[0] {
	case "keygen":
		return keygen(e)
	case "sign":
		return sign(args[1:], e)
	case "verify":
		return verify(args[1:], e)
	default:
		fmt.Fprintf(e.stderr, "release-sign: unknown subcommand %q\n", args[0])
		return 2
	}
}

func keygen(e env) int {
	if e.stdoutIsTerminal {
		fmt.Fprintf(e.stderr, "release-sign: keygen writes the private seed to stdout; pipe it instead, e.g. | gh secret set %s --env release\n", signingKeyEnv)
		return 2
	}
	pub, priv, err := ed25519.GenerateKey(e.rand)
	if err != nil {
		fmt.Fprintln(e.stderr, "release-sign: generate key:", err)
		return 1
	}
	if _, err := io.WriteString(e.stdout, base64.StdEncoding.EncodeToString(priv.Seed())); err != nil {
		fmt.Fprintln(e.stderr, "release-sign: write seed:", err)
		return 1
	}
	fmt.Fprintf(e.stderr, "public key (embed in internal/selfupdate trustedSigKeys): %s\n", base64.StdEncoding.EncodeToString(pub))
	return 0
}

// pathFlags parses the two required path flags of sign and verify.
func pathFlags(name string, args []string, e env, a, b string) (string, string, bool) {
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(e.stderr)
	av := fl.String(a, "", "path")
	bv := fl.String(b, "", "path")
	if err := fl.Parse(args); err != nil {
		return "", "", false
	}
	if *av == "" || *bv == "" || fl.NArg() != 0 {
		fmt.Fprintf(e.stderr, "release-sign: %s needs -%s and -%s\n", name, a, b)
		return "", "", false
	}
	return *av, *bv, true
}

func sign(args []string, e env) int {
	in, out, ok := pathFlags("sign", args, e, "in", "out")
	if !ok {
		return 2
	}
	seed := strings.TrimSpace(e.getenv(signingKeyEnv))
	if seed == "" {
		if len(e.trust) == 0 {
			fmt.Fprintf(e.stdout, "::warning::%s is not set and no signing key is embedded; %s ships unsigned\n", signingKeyEnv, in)
			return 0
		}
		fmt.Fprintf(e.stdout, "::error::%s is not set, but clients embed %d trusted key(s) and would refuse an unsigned release\n", signingKeyEnv, len(e.trust))
		return 1
	}
	// The decode error never carries the secret's bytes.
	raw, err := base64.StdEncoding.DecodeString(seed)
	if err != nil || len(raw) != ed25519.SeedSize {
		fmt.Fprintf(e.stdout, "::error::%s is not a base64 %d-byte ed25519 seed\n", signingKeyEnv, ed25519.SeedSize)
		return 1
	}
	priv := ed25519.NewKeyFromSeed(raw)
	if len(e.trust) > 0 && !trusted(priv.Public().(ed25519.PublicKey), e.trust) {
		fmt.Fprintf(e.stdout, "::error::the key in %s is not in the embedded trust set; clients would refuse its signature\n", signingKeyEnv)
		return 1
	}
	payload, err := os.ReadFile(in)
	if err != nil {
		fmt.Fprintln(e.stderr, "release-sign:", err)
		return 1
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload))
	if err := os.WriteFile(out, []byte(sig), 0o644); err != nil {
		fmt.Fprintln(e.stderr, "release-sign:", err)
		return 1
	}
	fmt.Fprintf(e.stdout, "release-sign: signed %s -> %s\n", in, out)
	return 0
}

func trusted(pub ed25519.PublicKey, set []ed25519.PublicKey) bool {
	for _, k := range set {
		if pub.Equal(k) {
			return true
		}
	}
	return false
}

func verify(args []string, e env) int {
	in, sigPath, ok := pathFlags("verify", args, e, "in", "sig")
	if !ok {
		return 2
	}
	sig, sigErr := os.ReadFile(sigPath)
	if len(e.trust) == 0 {
		switch {
		case errors.Is(sigErr, fs.ErrNotExist):
			fmt.Fprintf(e.stdout, "release-sign: no signing key embedded and no %s; nothing to verify\n", sigPath)
			return 0
		case sigErr != nil:
			fmt.Fprintln(e.stderr, "release-sign:", sigErr)
			return 1
		}
		fmt.Fprintf(e.stdout, "::warning::no signing key is embedded, so %s cannot be checked and clients ignore it\n", sigPath)
		return 0
	}
	if sigErr != nil {
		fmt.Fprintf(e.stdout, "::error::clients embed %d trusted key(s) and refuse a release without a valid signature: %v\n", len(e.trust), sigErr)
		return 1
	}
	payload, err := os.ReadFile(in)
	if err != nil {
		fmt.Fprintln(e.stderr, "release-sign:", err)
		return 1
	}
	idx, err := selfupdate.VerifyChecksumsSignature(payload, sig, e.trust)
	if err != nil {
		fmt.Fprintf(e.stdout, "::error::%s does not verify against the embedded trust set: %v\n", sigPath, err)
		return 1
	}
	fmt.Fprintf(e.stdout, "release-sign: %s verified by trusted key %d\n", sigPath, idx)
	return 0
}
