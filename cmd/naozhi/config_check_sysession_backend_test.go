package main

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/config"
)

func boolPtr(b bool) *bool { return &b }

// TestSysessionBackendDiags covers the combination that used to fail silently: a
// non-claude default backend with features that shell out using Claude's one-shot
// argv. `naozhi config check` now reports it statically, so the operator does not
// have to infer the cause from per-tick argv failures.
func TestSysessionBackendDiags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		backend   string
		sysession bool
		orient    *bool // nil = unset, which ImageOrientEnabled reads as TRUE
		wantKeys  []string
	}{
		{
			name: "claude default reports nothing", backend: "claude",
			sysession: true, orient: boolPtr(true), wantKeys: nil,
		},
		{
			name: "empty default means claude", backend: "",
			sysession: true, orient: boolPtr(true), wantKeys: nil,
		},
		{
			// Orient is on by default, so this is every kiro deployment, not only
			// the ones that opted into daemons.
			name: "kiro default with everything at its default", backend: "kiro",
			sysession: false, orient: nil,
			wantKeys: []string{"image_orient.enabled"},
		},
		{
			name: "kiro default with daemons on", backend: "kiro",
			sysession: true, orient: nil,
			wantKeys: []string{"sysession.enabled", "image_orient.enabled"},
		},
		{
			name: "kiro default with orient explicitly off", backend: "kiro",
			sysession: true, orient: boolPtr(false),
			wantKeys: []string{"sysession.enabled"},
		},
		{
			name: "kiro default with both off reports nothing", backend: "kiro",
			sysession: false, orient: boolPtr(false), wantKeys: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{}
			cfg.CLI.Backend = c.backend
			cfg.Sysession.Enabled = c.sysession
			cfg.ImageOrient.Enabled = c.orient

			got := sysessionBackendDiags(cfg)
			if len(got) != len(c.wantKeys) {
				t.Fatalf("got %d diags %+v, want keys %v", len(got), got, c.wantKeys)
			}
			for i, want := range c.wantKeys {
				if got[i].Key != want {
					t.Errorf("diag[%d].Key = %q, want %q", i, got[i].Key, want)
				}
				if got[i].Backend != c.backend {
					t.Errorf("diag[%d].Backend = %q, want %q", i, got[i].Backend, c.backend)
				}
				if got[i].Layer != "caps" {
					t.Errorf("diag[%d].Layer = %q, want caps", i, got[i].Layer)
				}
				// The reason must name both backends, or the operator cannot tell
				// what to change.
				if !strings.Contains(got[i].Reason, c.backend) || !strings.Contains(got[i].Reason, "claude") {
					t.Errorf("diag[%d].Reason = %q; must name both %q and claude", i, got[i].Reason, c.backend)
				}
			}
		})
	}
}
