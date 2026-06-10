package server

import (
	"strings"
	"testing"
)

// TestCronViewJS_NoActiveTabResidue pins the R20260610-CR-010 (#1999)
// cleanup: the 4-tab run-detail UI was replaced by the single-screen
// final-output view (cronTimelineDetailHtml), but
// cronTimelineFetchTranscript kept writing det.__activeTab /
// det.__activeTabUserSet that nothing read anymore — misleading state on
// the detail object. This test fails if anyone reintroduces the
// identifiers into any embedded static JS without also adding a reader.
func TestCronViewJS_NoActiveTabResidue(t *testing.T) {
	t.Parallel()
	for _, f := range []struct {
		fs   interface{ ReadFile(string) ([]byte, error) }
		path string
	}{
		{cronViewJS, "static/cron_view.js"},
		{dashboardJS, "static/dashboard.js"},
	} {
		data, err := f.fs.ReadFile(f.path)
		if err != nil {
			t.Fatalf("read %s: %v", f.path, err)
		}
		if strings.Contains(string(data), "__activeTab") {
			t.Errorf("%s: __activeTab residue found — the 4-tab run-detail UI was removed, this state has no readers (#1999)", f.path)
		}
	}
}
