package main

import (
	"strings"
	"testing"
)

func TestUpgradeDecision(t *testing.T) {
	cases := []struct {
		name            string
		latest, current string
		force           bool
		want            upgradeAction
		wantMsg         string
	}{
		{"newer proceeds", "v0.0.83", "v0.0.82", false, upgradeProceed, "New version available: v0.0.82 → v0.0.83"},
		{"equal is already latest", "v0.0.82", "v0.0.82", false, upgradeAlreadyLatest, "Already at the latest version (v0.0.82)"},
		{"equal with force is still already latest", "v0.0.82", "v0.0.82", true, upgradeAlreadyLatest, "Already at the latest"},
		{"older refused", "v0.0.81", "v0.0.82", false, upgradeRefuse, "Latest release v0.0.81 is not newer than running v0.0.82; use --force"},
		{"older with force proceeds", "v0.0.81", "v0.0.82", true, upgradeProceed, "installing it anyway (--force)"},
		{"build past the latest tag refused", "v0.0.82", "v0.0.82-3-gabc1234", false, upgradeRefuse, "not newer than running v0.0.82-3-gabc1234"},
		{"unparseable current refused", "v0.0.82", "abc1234", false, upgradeRefuse, "not newer than running abc1234"},
		{"unparseable latest refused", "nightly", "v0.0.82", false, upgradeRefuse, "Latest release nightly is not newer"},
		{"dev refused", "v0.0.82", "dev", false, upgradeRefuse, "Running a dev build. Use --force to replace it with release v0.0.82."},
		{"dev with force proceeds", "v0.0.82", "dev", true, upgradeProceed, "dev build — upgrading to v0.0.82 (--force)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := upgradeDecision(tc.latest, tc.current, tc.force)
			if got != tc.want {
				t.Errorf("upgradeDecision(%q, %q, %v) action = %d, want %d", tc.latest, tc.current, tc.force, got, tc.want)
			}
			if !strings.Contains(msg, tc.wantMsg) || !strings.HasSuffix(msg, "\n") {
				t.Errorf("message = %q, want it to contain %q and end in a newline", msg, tc.wantMsg)
			}
		})
	}
}
