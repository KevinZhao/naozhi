package midway

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExpiry(t *testing.T) {
	// Placeholder values only. Columns: domain, subdomains, path, secure,
	// expiry, name, value.
	const file = "# Netscape HTTP Cookie File\n" +
		"midway-auth.amazon.com\tFALSE\t/\tTRUE\t1825461286\tuser_name\tFIXTURE\n" +
		"#HttpOnly_.midway-auth.amazon.com\tTRUE\t/\tTRUE\t1793938520\tamazon_enterprise_access\tFIXTURE\n" +
		"#HttpOnly_midway-auth.amazon.com\tFALSE\t/\tTRUE\t1793997294\tsession\tFIXTURE\n" +
		"#HttpOnly_midway-auth.amazon.com\tFALSE\t/\tTRUE\t1793997000\t__Host-session\tFIXTURE\n" +
		"other.example.com\tFALSE\t/\tTRUE\t1900000000\tsession\tFIXTURE\n"
	dir := t.TempDir()
	for _, tc := range []struct {
		name, body string
		want       time.Time
	}{
		{"latest session cookie", file, time.Unix(1793997294, 0)},
		{"no session cookie", "midway-auth.amazon.com\tFALSE\t/\tTRUE\t1825461286\tuser_name\tFIXTURE\n", time.Time{}},
		{"empty", "", time.Time{}},
		{"short line", "midway-auth.amazon.com\tsession\n", time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := Expiry(path)
			if err != nil || !got.Equal(tc.want) {
				t.Errorf("Expiry = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	if got, err := Expiry(filepath.Join(dir, "missing")); err != nil || !got.IsZero() {
		t.Errorf("missing file: %v, %v", got, err)
	}
}
