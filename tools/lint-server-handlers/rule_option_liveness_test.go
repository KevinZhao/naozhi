package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeOptionsPkg(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "opts.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A field read via <param>.Field in a function taking the opts type by value
// is live; a field set only in a composite literal (the write side) is not;
// a field assigned to (never read) is not.
func TestScanOptionLiveness_HubOptions(t *testing.T) {
	t.Parallel()
	src := `package server

type HubOptions struct {
	Live     int
	WriteOnly int
	Dead     int
}

func NewHub(opts HubOptions) int {
	opts.WriteOnly = 1
	return opts.Live
}

func buildHub() HubOptions {
	return HubOptions{Live: 1, WriteOnly: 2, Dead: 3}
}

type sendEngineOpts struct {
	Live int
}

func newSendEngine(o sendEngineOpts) int { return o.Live }
`
	vs := scanOptionLiveness(writeOptionsPkg(t, src))
	var dead []string
	for _, v := range vs {
		if v.Rule != "option_liveness" {
			t.Errorf("unexpected rule: %+v", v)
			continue
		}
		dead = append(dead, strings.Fields(v.Message)[0])
	}
	if len(dead) != 2 {
		t.Fatalf("dead = %v, want 2 entries (WriteOnly, Dead)", dead)
	}
	for _, want := range []string{"HubOptions.Dead", "HubOptions.WriteOnly"} {
		found := false
		for _, d := range dead {
			if d == want {
				found = true
			}
		}
		if !found {
			t.Errorf("want %s reported dead, got %v", want, dead)
		}
	}
}

// Both HubOptions and sendEngineOpts are checked independently in one scan.
func TestScanOptionLiveness_BothTypes(t *testing.T) {
	t.Parallel()
	src := `package server

type HubOptions struct {
	Live int
}

func NewHub(opts HubOptions) int { return opts.Live }

type sendEngineOpts struct {
	Dead int
}

func newSendEngine(o sendEngineOpts) int { return 0 }
`
	vs := scanOptionLiveness(writeOptionsPkg(t, src))
	if len(vs) != 1 || !strings.Contains(vs[0].Message, "sendEngineOpts.Dead") {
		t.Fatalf("want exactly one sendEngineOpts.Dead violation, got %+v", vs)
	}
}

// A missing type is reported once, not cascaded into field-by-field noise.
func TestScanOptionLiveness_TypeNotFound(t *testing.T) {
	t.Parallel()
	src := "package server\n\nfunc noop() {}\n"
	vs := scanOptionLiveness(writeOptionsPkg(t, src))
	if len(vs) != 2 {
		t.Fatalf("want one 'not found' per missing type, got %d: %+v", len(vs), vs)
	}
	for _, v := range vs {
		if !strings.Contains(v.Message, "not found") {
			t.Errorf("want a 'not found' message, got %q", v.Message)
		}
	}
}
