//go:build !windows

package cli

import (
	"net"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// R20260527122801-SEC-1: enforceCLIPathSafe must REJECT a FIFO / socket
// cliPath at spawn time — the last-hop refusal before exec.Command sees the argv, so a
// `cli.path = /tmp/fifo` misconfig cannot deliver a file-type-confusion
// attack to the shim.
func TestEnforceCLIPathSafe_RejectsSpecialFiles(t *testing.T) {
	cases := []struct {
		name string
		make func(t *testing.T) string
	}{
		{"fifo", func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "fakecli.fifo")
			if err := syscall.Mkfifo(p, 0o600); err != nil {
				t.Skipf("mkfifo unavailable: %v", err)
			}
			return p
		}},
		{"socket", func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "s.sock")
			// macOS sun_path cap is ~104 bytes — skip if the temp path is too long.
			if len(p) >= 100 {
				t.Skipf("socket path too long for sun_path: %d", len(p))
			}
			l, err := net.Listen("unix", p)
			if err != nil {
				t.Skipf("unix listen unavailable: %v", err)
			}
			t.Cleanup(func() { l.Close() })
			return p
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.make(t)
			if err := enforceCLIPathSafe(p); err == nil || !strings.Contains(err.Error(), "regular file") {
				t.Fatalf("enforceCLIPathSafe(%q) = %v, want file-type refusal", p, err)
			}
		})
	}
}
