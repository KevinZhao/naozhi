package main

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/config"
)

func boolPtr(b bool) *bool { return &b }

// TestSysessionBackendDiags covers the combination that used to fail silently: a
// non-claude default backend with features that shell out using Claude's one-shot
// argv. `naozhi config check` now reports it statically, so the operator does not
// have to infer the cause from per-tick argv failures.
func TestSysessionBackendDiags(t *testing.T) {
	t.Parallel()
	backend.EnsureDefaults() // startupDefaultBackendID consults the registry
	cases := []struct {
		name      string
		backend   string
		backends  []string // cli.backends ids; nil = single-backend config
		sysession bool
		orient    *bool // nil = unset, which ImageOrientEnabled reads as TRUE
		wantKeys  []string
		// wantBackend is the backend the diags must name; "" means backend.
		wantBackend string
		// fallback: cli.backend/the first entry is not what binds, and the
		// reason must say startup fell back.
		fallback bool
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
		{
			// No cli.backend: the first cli.backends entry is the default, and
			// startup hands it to both features.
			name: "kiro listed first with no cli.backend", backends: []string{"kiro", "claude"},
			sysession: true, orient: nil,
			wantKeys:    []string{"sysession.enabled", "image_orient.enabled"},
			wantBackend: "kiro",
		},
		{
			name: "claude listed first with no cli.backend", backends: []string{"claude", "kiro"},
			sysession: true, orient: nil, wantKeys: nil,
		},
		{
			// cli.backend names no enabled entry: startup falls back to the
			// first registered one.
			name: "cli.backend claude absent from cli.backends", backend: "claude", backends: []string{"kiro"},
			sysession: true, orient: nil,
			wantKeys:    []string{"sysession.enabled", "image_orient.enabled"},
			wantBackend: "kiro", fallback: true,
		},
		{
			// Unknown ids are skipped at startup, so kiro is what binds.
			name: "unknown id ahead of kiro", backends: []string{"definitely-not-a-backend", "kiro"},
			sysession: false, orient: nil,
			wantKeys:    []string{"image_orient.enabled"},
			wantBackend: "kiro", fallback: true,
		},
		{
			name: "explicit kiro among several", backend: "kiro", backends: []string{"kiro", "claude"},
			sysession: true, orient: boolPtr(false),
			wantKeys: []string{"sysession.enabled"},
		},
		{
			name: "explicit claude listed second", backend: "claude", backends: []string{"kiro", "claude"},
			sysession: true, orient: nil, wantKeys: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{}
			cfg.CLI.Backend = c.backend
			for _, id := range c.backends {
				cfg.CLI.Backends = append(cfg.CLI.Backends, config.CLIBackendConfig{ID: id, Path: "/tmp/never-" + id})
			}
			cfg.Sysession.Enabled = c.sysession
			cfg.ImageOrient.Enabled = c.orient

			got := sysessionBackendDiags(cfg)
			wantBackend := c.wantBackend
			if wantBackend == "" {
				wantBackend = c.backend
			}
			if len(got) != len(c.wantKeys) {
				t.Fatalf("got %d diags %+v, want keys %v", len(got), got, c.wantKeys)
			}
			for i, want := range c.wantKeys {
				if got[i].Key != want {
					t.Errorf("diag[%d].Key = %q, want %q", i, got[i].Key, want)
				}
				if got[i].Backend != wantBackend {
					t.Errorf("diag[%d].Backend = %q, want %q", i, got[i].Backend, wantBackend)
				}
				if got[i].Layer != "caps" {
					t.Errorf("diag[%d].Layer = %q, want caps", i, got[i].Layer)
				}
				// The reason must name both backends, or the operator cannot tell
				// what to change.
				if !strings.Contains(got[i].Reason, wantBackend) || !strings.Contains(got[i].Reason, "claude") {
					t.Errorf("diag[%d].Reason = %q; must name both %q and claude", i, got[i].Reason, wantBackend)
				}
				if mentions := strings.Contains(got[i].Reason, "falls back"); mentions != c.fallback {
					t.Errorf("diag[%d].Reason = %q; mentions fallback = %v, want %v", i, got[i].Reason, mentions, c.fallback)
				}
			}
		})
	}
}
