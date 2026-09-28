package selfupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	testLeaf  = "d015996bfd2d7436d44bcd59f9103a9d007fa554"
	testLeafR = `designated => identifier "com.naozhi.agent" and certificate leaf = H"` + testLeaf + `"`
)

func TestParseDesignatedRequirement(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		out  string
		kind CodesignKind
		id   CodesignIdentity
	}{
		{"leaf", "Executable=/x/naozhi\n" + testLeafR + "\n", CodesignLeaf,
			CodesignIdentity{Identifier: "com.naozhi.agent", LeafSHA1: testLeaf}},
		{"leaf uppercase hash is normalised",
			`designated => identifier "a.b" and certificate leaf = H"D015996BFD2D7436D44BCD59F9103A9D007FA554"`,
			CodesignLeaf, CodesignIdentity{Identifier: "a.b", LeafSHA1: testLeaf}},
		// Real codesign output: the ad-hoc requirement is emitted as a "# " comment.
		{"adhoc", "Executable=/x\n# designated => cdhash H\"a523778a486a8c9c8e27db684864fba7fd06f4e4\"\n", CodesignAdhoc, CodesignIdentity{}},
		{"developer id is left alone",
			`designated => anchor apple generic and identifier "com.x" and (certificate leaf[field.1.2.840.113635.100.6.1.9] /* exists */)`,
			CodesignOther, CodesignIdentity{}},
		{"leaf plus extra clause is not reproducible",
			testLeafR + ` and info [CFBundleVersion] = "1"`, CodesignOther, CodesignIdentity{}},
		{"unsigned", "/x: code object is not signed at all\n", CodesignUnknown, CodesignIdentity{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, id := parseDesignatedRequirement(c.out)
			if kind != c.kind || id != c.id {
				t.Errorf("got (%d, %+v), want (%d, %+v)", kind, id, c.kind, c.id)
			}
		})
	}
}

// fakeCodesign simulates codesign over a path → requirement table.
type fakeCodesign struct {
	dr     map[string]string
	signs  [][]string
	onSign func(path string) error // nil: succeed and record the leaf requirement
}

func (f *fakeCodesign) run(_ context.Context, args ...string) ([]byte, error) {
	path := args[len(args)-1]
	if args[0] == "-d" {
		if r, ok := f.dr[path]; ok {
			return []byte("Executable=" + path + "\n" + r + "\n"), nil
		}
		return []byte(path + ": code object is not signed at all"), errors.New("exit status 1")
	}
	f.signs = append(f.signs, args)
	if f.onSign != nil {
		if err := f.onSign(path); err != nil {
			return []byte("boom"), err
		}
		return nil, nil
	}
	f.dr[path] = `designated => identifier "` + args[4] + `" and certificate leaf = H"` + args[2] + `"`
	return nil, nil
}

// withFakeCodesign swaps the package hooks; callers must not be t.Parallel.
func withFakeCodesign(t *testing.T, f *fakeCodesign) {
	t.Helper()
	origRun, origSupported := codesignRun, codesignSupported
	codesignRun, codesignSupported = f.run, true
	t.Cleanup(func() { codesignRun, codesignSupported = origRun, origSupported })
}

func replaceFixture(t *testing.T) (installPath, newBin string) {
	t.Helper()
	dir := t.TempDir()
	installPath = filepath.Join(dir, "naozhi")
	newBin = filepath.Join(dir, "naozhi-new")
	if err := os.WriteFile(installPath, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newBin, []byte("new binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return installPath, newBin
}

func TestReplace_ResignsWithInstalledLeafIdentity(t *testing.T) {
	installPath, newBin := replaceFixture(t)
	f := &fakeCodesign{dr: map[string]string{installPath: testLeafR}}
	withFakeCodesign(t, f)

	if _, err := Replace(newBin, installPath); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if len(f.signs) != 1 {
		t.Fatalf("sign calls = %v, want exactly one", f.signs)
	}
	got := strings.Join(f.signs[0][:5], " ")
	if want := "-f -s " + testLeaf + " --identifier com.naozhi.agent"; got != want {
		t.Errorf("sign argv = %q, want %q", got, want)
	}
	// Signed the staging file, never the live path or the download.
	if p := f.signs[0][5]; p == installPath || p == newBin || filepath.Dir(p) != filepath.Dir(installPath) {
		t.Errorf("signed %q; want a staging file beside %q", p, installPath)
	}
	if b, _ := os.ReadFile(installPath); string(b) != "new binary" {
		t.Errorf("installed = %q", b)
	}
}

func TestReplace_AdhocInstallIsNotResigned(t *testing.T) {
	installPath, newBin := replaceFixture(t)
	f := &fakeCodesign{dr: map[string]string{installPath: `designated => cdhash H"a523778a486a8c9c8e27db684864fba7fd06f4e4"`}}
	withFakeCodesign(t, f)

	if _, err := Replace(newBin, installPath); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if len(f.signs) != 0 {
		t.Errorf("ad-hoc install must not be re-signed; got %v", f.signs)
	}
}

func TestReplace_UnsupportedPlatformNeverRunsCodesign(t *testing.T) {
	installPath, newBin := replaceFixture(t)
	calls := 0
	origRun, origSupported := codesignRun, codesignSupported
	codesignRun = func(context.Context, ...string) ([]byte, error) { calls++; return nil, nil }
	codesignSupported = false
	t.Cleanup(func() { codesignRun, codesignSupported = origRun, origSupported })

	if _, err := Replace(newBin, installPath); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if calls != 0 {
		t.Errorf("codesign ran %d times off darwin", calls)
	}
}

// A codesign that dies mid-write must not put its half-written file live: the
// pristine download is re-staged and the upgrade still succeeds (ad-hoc).
func TestReplace_FailedSignRestagesPristineBinary(t *testing.T) {
	installPath, newBin := replaceFixture(t)
	f := &fakeCodesign{dr: map[string]string{installPath: testLeafR}}
	f.onSign = func(path string) error {
		_ = os.WriteFile(path, []byte("half-written"), 0o600)
		return errors.New("exit status 1")
	}
	withFakeCodesign(t, f)

	if _, err := Replace(newBin, installPath); err != nil {
		t.Fatalf("Replace must survive a signing failure: %v", err)
	}
	if b, _ := os.ReadFile(installPath); string(b) != "new binary" {
		t.Errorf("installed = %q, want the pristine new binary", b)
	}
}

// Exit 0 with a different requirement would still lose every grant.
func TestReplace_SignWithWrongResultIsTreatedAsFailure(t *testing.T) {
	installPath, newBin := replaceFixture(t)
	f := &fakeCodesign{dr: map[string]string{installPath: testLeafR}}
	f.onSign = func(path string) error {
		_ = os.WriteFile(path, []byte("resigned"), 0o600)
		f.dr[path] = `designated => cdhash H"00"`
		return nil
	}
	withFakeCodesign(t, f)

	if _, err := Replace(newBin, installPath); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if b, _ := os.ReadFile(installPath); string(b) != "new binary" {
		t.Errorf("installed = %q, want the re-staged pristine binary", b)
	}
}

// If even re-staging fails the staged bytes are unknown: abort, keep the old
// binary, leave no backup or staging litter.
func TestReplace_FailedRestageAbortsAndKeepsOldBinary(t *testing.T) {
	installPath, newBin := replaceFixture(t)
	f := &fakeCodesign{dr: map[string]string{installPath: testLeafR}}
	f.onSign = func(path string) error {
		_ = os.Remove(newBin) // restore copies from here
		return errors.New("exit status 1")
	}
	withFakeCodesign(t, f)

	if _, err := Replace(newBin, installPath); err == nil {
		t.Fatal("Replace must fail when the pristine binary cannot be re-staged")
	}
	if b, _ := os.ReadFile(installPath); string(b) != "old binary" {
		t.Errorf("installed = %q, want the untouched old binary", b)
	}
	entries, _ := os.ReadDir(filepath.Dir(installPath))
	for _, e := range entries {
		if e.Name() != "naozhi" {
			t.Errorf("leftover %q after aborted Replace", e.Name())
		}
	}
}

// The real codesign on a real Go test binary: darwin/arm64 links ad-hoc.
func TestInspectCodesign_RealAdhocBinary(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("ad-hoc linker signature is a darwin/arm64 property")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if kind, _ := InspectCodesign(exe); kind != CodesignAdhoc {
		t.Errorf("InspectCodesign(test binary) = %d, want CodesignAdhoc", kind)
	}
}
