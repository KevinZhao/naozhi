package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/selfupdate"
)

// Not t.Parallel: swaps package-level hooks.
func TestDoctor_CheckCodesign(t *testing.T) {
	cases := []struct {
		name   string
		goos   string
		kind   selfupdate.CodesignKind
		level  string
		detail string
	}{
		{"not darwin", "linux", selfupdate.CodesignUnknown, "pass", "skipped"},
		{"leaf", "darwin", selfupdate.CodesignLeaf, "pass", "identifier=com.naozhi.agent"},
		{"developer id", "darwin", selfupdate.CodesignOther, "pass", "non-ad-hoc"},
		{"adhoc", "darwin", selfupdate.CodesignAdhoc, "warn", "docs/ops/macos-codesign.md"},
		{"unreadable", "darwin", selfupdate.CodesignUnknown, "warn", "cannot read"},
	}
	origGOOS, origInspect := codesignGOOS, inspectCodesignFn
	t.Cleanup(func() { codesignGOOS, inspectCodesignFn = origGOOS, origInspect })
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			codesignGOOS = c.goos
			inspectCodesignFn = func(string) (selfupdate.CodesignKind, selfupdate.CodesignIdentity) {
				return c.kind, selfupdate.CodesignIdentity{Identifier: "com.naozhi.agent", LeafSHA1: "d015"}
			}
			d := &doctor{out: &bytes.Buffer{}}
			d.checkCodesign()
			if len(d.findings) != 1 {
				t.Fatalf("findings = %+v", d.findings)
			}
			f := d.findings[0]
			if f.Category != "codesign" || f.Level != c.level || !strings.Contains(f.Detail, c.detail) {
				t.Errorf("got %+v, want level=%s detail~%q", f, c.level, c.detail)
			}
			if d.hasFail {
				t.Error("codesign must never fail doctor: an ad-hoc binary still works")
			}
		})
	}
}
