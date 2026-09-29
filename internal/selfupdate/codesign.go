// codesign.go — keeps a macOS binary's signing identity stable across upgrades.
//
// macOS privacy grants (Full Disk Access, Documents/Desktop, "files managed by
// OneDrive" …) are keyed to the binary's designated requirement. Release
// binaries are ad-hoc signed, whose requirement is the cdhash, so every
// upgrade invalidates every grant and the prompts come back. An operator who
// re-signed the install with a self-signed certificate (docs/ops/macos-codesign.md)
// gets `identifier "X" and certificate leaf = H"…"` instead — stable only if
// each upgrade is signed the same way, which is what preserveCodesign does.
package selfupdate

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// CodesignKind classifies a binary's designated requirement.
type CodesignKind int

const (
	CodesignUnknown CodesignKind = iota // unsigned, unreadable, or not darwin
	CodesignAdhoc                       // cdhash requirement: changes every build
	CodesignLeaf                        // identifier + certificate leaf: what we preserve
	CodesignOther                       // e.g. Developer ID; stable on its own, left alone
)

// CodesignIdentity is the part of a leaf requirement a re-sign must reproduce.
type CodesignIdentity struct {
	Identifier string
	LeafSHA1   string // hex; `codesign -s` accepts it as the identity
}

// codesignTimeout bounds each codesign run: a keychain ACL dialog nobody
// answers would otherwise hold installMu forever.
const codesignTimeout = 60 * time.Second

// codesignSupported and codesignRun are indirected so tests need neither
// darwin nor a keychain.
var codesignSupported = runtime.GOOS == "darwin"

var codesignRun = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, resolveTrustedBin("codesign"), args...).CombinedOutput()
}

var (
	leafDRRe  = regexp.MustCompile(`^designated => identifier "([^"]+)" and certificate leaf = H"([0-9A-Fa-f]{40})"$`)
	adhocDRRe = regexp.MustCompile(`^designated => cdhash H"[0-9A-Fa-f]+"$`)
)

// parseDesignatedRequirement reads `codesign -d -r-` output. Only the exact
// two-clause leaf form counts as CodesignLeaf: anything richer (anchors,
// certificate chains) is a requirement we could not reproduce faithfully.
func parseDesignatedRequirement(out string) (CodesignKind, CodesignIdentity) {
	for _, line := range strings.Split(out, "\n") {
		// codesign prints a synthesized requirement (ad-hoc's cdhash) as a "# " comment.
		line = strings.TrimPrefix(strings.TrimSpace(line), "# ")
		if !strings.HasPrefix(line, "designated => ") {
			continue
		}
		if m := leafDRRe.FindStringSubmatch(line); m != nil {
			return CodesignLeaf, CodesignIdentity{Identifier: m[1], LeafSHA1: strings.ToLower(m[2])}
		}
		if adhocDRRe.MatchString(line) {
			return CodesignAdhoc, CodesignIdentity{}
		}
		return CodesignOther, CodesignIdentity{}
	}
	return CodesignUnknown, CodesignIdentity{}
}

// InspectCodesign reports how path is signed; CodesignUnknown off darwin.
func InspectCodesign(path string) (CodesignKind, CodesignIdentity) {
	if !codesignSupported {
		return CodesignUnknown, CodesignIdentity{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), codesignTimeout)
	defer cancel()
	out, err := codesignRun(ctx, "-d", "-r-", path)
	if err != nil {
		return CodesignUnknown, CodesignIdentity{}
	}
	return parseDesignatedRequirement(string(out))
}

// preserveCodesign signs stagePath with installPath's leaf identity before it
// goes live. Best effort by design: a failure leaves the release's ad-hoc
// signature (prompts return, nothing breaks), so it warns instead of failing
// the upgrade. restore re-stages the pristine binary in case codesign died
// halfway through rewriting the file; only ITS failure is returned, because
// then the staged bytes are unknown and must not go live.
func preserveCodesign(installPath, stagePath string, restore func() error) error {
	kind, id := InspectCodesign(installPath)
	if kind != CodesignLeaf {
		return nil
	}
	err := signWithIdentity(stagePath, id)
	if err == nil {
		slog.Info("selfupdate: re-signed new binary with the installed identity",
			"identifier", id.Identifier, "leaf", id.LeafSHA1)
		return nil
	}
	slog.Warn("selfupdate: could not keep the code-signing identity; macOS will ask for folder access again",
		"identifier", id.Identifier, "leaf", id.LeafSHA1, "err", err,
		"fix", fmt.Sprintf("codesign -f -s %s --identifier %s %s", id.LeafSHA1, id.Identifier, installPath))
	if rerr := restore(); rerr != nil {
		return fmt.Errorf("re-stage new binary after failed codesign: %w", rerr)
	}
	return nil
}

// signWithIdentity signs path and confirms the result carries exactly id — a
// success exit with some other requirement would still lose the grants.
func signWithIdentity(path string, id CodesignIdentity) error {
	ctx, cancel := context.WithTimeout(context.Background(), codesignTimeout)
	defer cancel()
	if out, err := codesignRun(ctx, "-f", "-s", id.LeafSHA1, "--identifier", id.Identifier, path); err != nil {
		return fmt.Errorf("codesign: %w: %s", err, strings.TrimSpace(string(out)))
	}
	kind, got := InspectCodesign(path)
	if kind != CodesignLeaf || got != id {
		return fmt.Errorf("re-signed binary has a different requirement (kind=%d identifier=%q leaf=%s)",
			kind, got.Identifier, got.LeafSHA1)
	}
	return nil
}
