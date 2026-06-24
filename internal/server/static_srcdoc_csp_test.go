package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_SrcdocCSP pins [R202606j-SEC-1] (#2341): the mobile/WebKit
// srcdoc fallback in renderSandboxedBlob must prepend a self-contained
// <meta http-equiv="Content-Security-Policy"> to the workspace HTML so the
// preview document does NOT inherit the dashboard page CSP. Without it, the
// srcdoc document runs under the dashboard's own script-src / connect-src
// allowlist, letting hostile workspace HTML reach dashboard-allowlisted
// origins. A <meta> CSP can only further-restrict the inherited policy, so
// it is strictly safe; the assertion windows on the srcdoc assignment so an
// unrelated iframe elsewhere can't satisfy it.
func TestDashboardJS_SrcdocCSP(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	idx := strings.Index(js, "frame.srcdoc")
	if idx < 0 {
		t.Fatal("srcdoc fallback assignment not found in dashboard.js")
	}
	// Window: from the srcdoc assignment back to the preceding CSP literal
	// definition (must be within the same render branch, a few lines above).
	start := idx - 1200
	if start < 0 {
		start = 0
	}
	block := js[start : idx+len("frame.srcdoc = sandboxCsp + new TextDecoder('utf-8').decode(bytes);")]

	if !strings.Contains(block, "http-equiv=\\\"Content-Security-Policy\\\"") {
		t.Error("srcdoc fallback must prepend a <meta http-equiv=\"Content-Security-Policy\"> to override the inherited dashboard CSP ([R202606j-SEC-1], #2341)")
	}
	// connect-src must be locked down so the preview cannot exfiltrate via
	// fetch/XHR/WebSocket using the dashboard's connect-src budget.
	if !strings.Contains(block, "connect-src 'none'") {
		t.Error("srcdoc CSP must set connect-src 'none' to block exfiltration ([R202606j-SEC-1], #2341)")
	}
	// The srcdoc content must actually be prefixed with the CSP literal, not
	// just decoded bytes (the bug was a bare decode with no CSP).
	if !strings.Contains(block, "sandboxCsp +") {
		t.Error("srcdoc must be prefixed with the sandboxCsp literal ([R202606j-SEC-1], #2341)")
	}
}
