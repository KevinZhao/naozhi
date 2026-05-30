package osutil

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestPathUnderRoot(t *testing.T) {
	sep := string(filepath.Separator)
	root := sep + "data" + sep + "ws"
	cases := []struct {
		name     string
		resolved string
		want     bool
	}{
		{"equal", root, true},
		{"child", root + sep + "proj", true},
		{"deep child", root + sep + "a" + sep + "b", true},
		{"sibling prefix not under", root + "-evil", false},
		{"outside", sep + "etc", false},
		{"parent", sep + "data", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PathUnderRoot(tc.resolved, root); got != tc.want {
				t.Errorf("PathUnderRoot(%q, %q) = %v, want %v", tc.resolved, root, got, tc.want)
			}
		})
	}
}

func TestResolveWorkspaceUnderRoot(t *testing.T) {
	sep := string(filepath.Separator)
	root := sep + "data" + sep + "ws"
	rootResolved := sep + "mnt" + sep + "ws" // root symlinks to here

	// identity resolver: returns input unchanged, no error.
	identity := func(p string) (string, error) { return p, nil }
	// failing resolver: every EvalSymlinks fails.
	failAll := func(p string) (string, error) { return "", errors.New("boom") }
	// rootFails resolves workDir but fails the root; used to exercise the
	// allowedRootResolved fallback.
	rootFails := func(p string) (string, error) {
		if p == root {
			return "", errors.New("root gone")
		}
		return p, nil
	}

	t.Run("empty workdir = no constraint", func(t *testing.T) {
		got, ok := ResolveWorkspaceUnderRoot("", root, "", identity)
		if got != "" || !ok {
			t.Fatalf("got (%q,%v), want (\"\",true)", got, ok)
		}
	})
	t.Run("empty root = disabled", func(t *testing.T) {
		got, ok := ResolveWorkspaceUnderRoot(root+sep+"p", "", "", identity)
		if got != "" || !ok {
			t.Fatalf("got (%q,%v), want (\"\",true)", got, ok)
		}
	})
	t.Run("relative workdir rejected", func(t *testing.T) {
		got, ok := ResolveWorkspaceUnderRoot("rel/path", root, "", identity)
		if got != "" || ok {
			t.Fatalf("got (%q,%v), want (\"\",false)", got, ok)
		}
	})
	t.Run("workdir under root", func(t *testing.T) {
		wd := root + sep + "proj"
		got, ok := ResolveWorkspaceUnderRoot(wd, root, "", identity)
		if !ok || got != wd {
			t.Fatalf("got (%q,%v), want (%q,true)", got, ok, wd)
		}
	})
	t.Run("workdir outside root", func(t *testing.T) {
		got, ok := ResolveWorkspaceUnderRoot(sep+"etc", root, "", identity)
		if got != "" || ok {
			t.Fatalf("got (%q,%v), want (\"\",false)", got, ok)
		}
	})
	t.Run("workdir EvalSymlinks fails => reject", func(t *testing.T) {
		got, ok := ResolveWorkspaceUnderRoot(root+sep+"p", root, "", failAll)
		if got != "" || ok {
			t.Fatalf("got (%q,%v), want (\"\",false)", got, ok)
		}
	})
	t.Run("root resolve fails, no cached fallback => reject", func(t *testing.T) {
		got, ok := ResolveWorkspaceUnderRoot(rootResolved+sep+"p", root, "", rootFails)
		if got != "" || ok {
			t.Fatalf("got (%q,%v), want (\"\",false)", got, ok)
		}
	})
	t.Run("root resolve fails, cached fallback admits child", func(t *testing.T) {
		wd := rootResolved + sep + "p"
		got, ok := ResolveWorkspaceUnderRoot(wd, root, rootResolved, rootFails)
		if !ok || got != wd {
			t.Fatalf("got (%q,%v), want (%q,true)", got, ok, wd)
		}
	})
}
