package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_SandboxedPreviewViaEndpoint pins the #1980 preview
// architecture that replaced the blob:/srcdoc delivery (and with it the
// [R202606j-SEC-1] #2341 meta-CSP patch): blob: and srcdoc iframe documents
// INHERIT the parent page CSP in current engines (measured in
// docs/rfc/csp-data-action.md §4), so after script-src dropped
// 'unsafe-inline' both paths would silently stop executing workspace HTML.
// renderSandboxedBlob must instead point the sandboxed iframe's src at the
// server's inline render form — the iframe document's policy then comes from
// that response's own `Content-Security-Policy: sandbox allow-scripts …`
// header (opaque origin, no connect budget), enforced server-side by
// TestHandleFileGet_RenderInlineIframeOnly.
func TestDashboardJS_SandboxedPreviewViaEndpoint(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	idx := strings.Index(js, "function renderSandboxedBlob(")
	if idx < 0 {
		t.Fatal("renderSandboxedBlob not found in dashboard.js")
	}
	end := strings.Index(js[idx:], "\n}")
	if end < 0 {
		t.Fatal("could not bound renderSandboxedBlob body")
	}
	body := js[idx : idx+end]

	if !strings.Contains(body, "'render') + '&inline=1'") {
		t.Error("renderSandboxedBlob must point the iframe at mode=render&inline=1 (its response carries its own sandbox CSP)")
	}
	if !strings.Contains(body, "sandbox', 'allow-scripts'") {
		t.Error("renderSandboxedBlob must keep the allow-scripts-only iframe sandbox attribute (belt to the response CSP)")
	}
	// The CSP-inheriting delivery shapes must not come back: srcdoc anywhere
	// in the file, and Blob/createObjectURL inside this helper.
	if strings.Contains(js, "srcdoc") {
		t.Error("dashboard.js reintroduced a srcdoc preview path — srcdoc documents inherit the page CSP, which has no script-src 'unsafe-inline' anymore")
	}
	for _, tok := range []string{"new Blob(", "createObjectURL"} {
		if strings.Contains(body, tok) {
			t.Errorf("renderSandboxedBlob reintroduced %q — blob: documents inherit the page CSP (docs/rfc/csp-data-action.md §4)", tok)
		}
	}
}
