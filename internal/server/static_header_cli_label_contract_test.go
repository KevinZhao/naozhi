// static_header_cli_label_contract_test.go — #2437: updateHeaderCLI must not
// clobber the #header-model tuning anchor.
//
// renderMainShell paints `.detail-left` as cliLabel + modelLabel, where the
// modelLabel span (#header-model, data-action="tuning-model") is what the
// model tuning popover anchors to. updateHeaderCLI runs on every
// /api/sessions poll that passes the version short-circuit; it used to
// assign `.detail-left`'s innerHTML to just "name vVersion", which erased
// #header-model (model stopped showing, switch-model entry vanished) and
// contradicted the D5 rule that the version lives only in the hover title.
// Source-grep pins per the #388 guidance in static_ux_contract_test.go.
package server

import (
	"strings"
	"testing"
)

func TestDashboardJS_UpdateHeaderCLI_PreservesHeaderModel(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	upd := jsFuncBody(t, js, "updateHeaderCLI")
	if strings.Contains(upd, ".detail-left") {
		t.Error("updateHeaderCLI must not target .detail-left — rewriting it drops the sibling #header-model span (#2437)")
	}
	if strings.Contains(upd, "innerHTML") {
		t.Error("updateHeaderCLI must not assign innerHTML — update the #header-cli span's text/title in place (#2437)")
	}
	if !strings.Contains(upd, "getElementById('header-cli')") {
		t.Error("updateHeaderCLI must locate the cli label by its fixed id #header-cli")
	}
	// D5 display rule: the version string belongs in the hover title only.
	if strings.Contains(upd, "' v' + esc(") {
		t.Error("updateHeaderCLI renders the version into the label text; renderMainShell keeps it title-only (D5)")
	}

	// mainHeaderHtml (renderMainShell's header builder) must emit the anchor
	// updateHeaderCLI targets and keep
	// #header-model right next to it.
	shell := jsFuncBody(t, js, "mainHeaderHtml")
	if !strings.Contains(shell, "headerCLILabelHtml(") {
		t.Error("mainHeaderHtml must build the cli label via headerCLILabelHtml so both writers share one markup shape")
	}
	if !strings.Contains(shell, `id="header-model"`) {
		t.Error("mainHeaderHtml must still paint #header-model (tuning popover anchor)")
	}
	helper := jsFuncBody(t, js, "headerCLILabelHtml")
	if !strings.Contains(helper, `id="header-cli"`) {
		t.Error("headerCLILabelHtml must stamp id=\"header-cli\" on the cli span")
	}
}
