package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestPlaceHelper(t *testing.T) {
	dir := t.TempDir()
	install := filepath.Join(dir, "bin", "naozhi")
	if err := os.MkdirAll(filepath.Dir(install), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(install, []byte("naozhi"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := filepath.Join(dir, helperAsset())
	body := []byte("helper bytes")
	if err := os.WriteFile(got, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	sumPath := filepath.Join(dir, "checksums.txt")
	write := func(hash string) {
		if err := os.WriteFile(sumPath, []byte(hash+"  "+helperAsset()+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(hex.EncodeToString(make([]byte, 32)))
	if err := placeHelper(got, sumPath, helperAsset(), install); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	dst := filepath.Join(filepath.Dir(install), HelperName)
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("helper installed despite mismatch: %v", err)
	}
	write(hex.EncodeToString(sum[:]))
	if err := placeHelper(got, sumPath, helperAsset(), install); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dst)
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("installed helper: %v %v", st, err)
	}
	if b, _ := os.ReadFile(dst); string(b) != string(body) {
		t.Fatalf("helper content %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(install), ".naozhi-upgrade-*")); len(left) != 0 {
		t.Fatalf("staging left behind: %v", left)
	}
}
