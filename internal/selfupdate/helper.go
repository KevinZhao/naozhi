package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// HelperName is the macOS meeting-audio helper (tools/naozhi-audio) installed beside naozhi.
const HelperName = "naozhi-audio"

// helperIdentifier is the helper's code-signing identifier: its TCC grants (microphone,
// system audio recording) are keyed to it and the signing certificate, so it must not change.
const helperIdentifier = "com.naozhi.audio"

// errNoHelperAsset means the release ships no helper for this platform.
var errNoHelperAsset = errors.New("release has no naozhi-audio for this platform")

func helperAsset() string { return fmt.Sprintf("%s-%s-%s", HelperName, runtime.GOOS, runtime.GOARCH) }

// InstallHelper puts the release's naozhi-audio next to installPath (macOS only). dir is the
// directory Download verified checksums.txt into. The helper is signed with the installed
// naozhi's self-signed certificate under its own identifier, so the two grants survive the
// upgrade like naozhi's do. Best effort: the gateway runs without it, so a failure only warns
// and leaves any installed helper as it was.
func InstallHelper(ctx context.Context, rel *Release, dir, installPath string) {
	if runtime.GOOS != "darwin" || rel == nil {
		return
	}
	if err := installHelper(ctx, rel, dir, installPath); err != nil && !errors.Is(err, errNoHelperAsset) {
		slog.Warn("selfupdate: naozhi-audio not updated; meeting recording keeps the installed one", "err", err)
	}
}

func installHelper(ctx context.Context, rel *Release, dir, installPath string) error {
	sumPath := filepath.Join(dir, "checksums.txt")
	sums, err := os.ReadFile(sumPath)
	if err != nil {
		return err
	}
	asset := helperAsset()
	if !strings.Contains(string(sums), " "+asset) {
		return errNoHelperAsset
	}
	url, ok := strings.CutSuffix(rel.AssetURL, assetName())
	if !ok {
		return fmt.Errorf("unexpected asset URL %q", rel.AssetURL)
	}
	got := filepath.Join(dir, asset)
	if err := fetchFile(ctx, url+asset, got, maxBinaryBytes); err != nil {
		return fmt.Errorf("download %s: %w", asset, err)
	}
	return placeHelper(got, sumPath, asset, installPath)
}

// placeHelper verifies the downloaded helper, signs it like the installed naozhi and renames it
// into place beside it.
func placeHelper(got, sumPath, asset, installPath string) error {
	if err := verifyChecksum(got, sumPath, asset); err != nil {
		return err
	}
	if err := os.Chmod(got, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(filepath.Dir(installPath), HelperName)
	stage, err := os.CreateTemp(filepath.Dir(installPath), stagingPattern)
	if err != nil {
		return err
	}
	stagePath := stage.Name()
	stage.Close()
	defer os.Remove(stagePath)
	if err := copyFile(got, stagePath); err != nil {
		return err
	}
	if kind, id := InspectCodesign(installPath); kind == CodesignLeaf {
		id.Identifier = helperIdentifier
		if err := signWithIdentity(stagePath, id); err != nil {
			return fmt.Errorf("sign naozhi-audio: %w (fix: codesign -f -s %s --identifier %s %s)", err, id.LeafSHA1, helperIdentifier, dst)
		}
	} else {
		slog.Warn("selfupdate: naozhi is not signed with a self-signed certificate; naozhi-audio keeps the release signature and macOS will ask for its two grants again after each upgrade (docs/ops/macos-codesign.md)")
	}
	if err := os.Chmod(stagePath, 0o755); err != nil {
		return err
	}
	return os.Rename(stagePath, dst)
}
