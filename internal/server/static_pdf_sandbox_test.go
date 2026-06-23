package server

import (
	"strings"
	"testing"
)

// pdfIframeBlock returns the substring of js spanning the
// `application/pdf`/`'pdf'` preview branch up to its `appendChild` so the
// sandbox assertion is windowed to the PDF code path (not an unrelated
// iframe elsewhere in the file).
func pdfIframeBlock(t *testing.T, js, anchor string) string {
	t.Helper()
	idx := strings.Index(js, anchor)
	if idx < 0 {
		t.Fatalf("PDF preview anchor %q not found", anchor)
	}
	rest := js[idx:]
	end := strings.Index(rest, "appendChild")
	if end < 0 {
		t.Fatalf("appendChild not found after anchor %q", anchor)
	}
	return rest[:end]
}

// TestDashboardJS_PDFIframe_Sandboxed pins [R202606g-SEC-4]: the PDF
// preview iframe in dashboard.js must carry a sandbox attribute. serveRaw
// forces application/pdf to an attachment download today, but if a proxy
// strips Content-Disposition (or serveRaw later renders PDF inline) an
// un-sandboxed iframe would let an embedded PDF plugin execute JS
// same-origin to the dashboard. sandbox="" is the defense-in-depth.
func TestDashboardJS_PDFIframe_Sandboxed(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	block := pdfIframeBlock(t, string(data), "mime === 'application/pdf'")
	if !strings.Contains(block, "sandbox") {
		t.Error("dashboard.js PDF preview iframe must set a sandbox attribute (defense-in-depth, [R202606g-SEC-4])")
	}
}

// TestFilesViewJS_PDFIframe_Sandboxed pins the same invariant for the
// workspace files browser (files_view.js).
func TestFilesViewJS_PDFIframe_Sandboxed(t *testing.T) {
	t.Parallel()
	data, err := filesViewJS.ReadFile("static/files_view.js")
	if err != nil {
		t.Fatalf("read files_view.js: %v", err)
	}
	block := pdfIframeBlock(t, string(data), "ext === 'pdf'")
	if !strings.Contains(block, "sandbox") {
		t.Error("files_view.js PDF preview iframe must set a sandbox attribute (defense-in-depth, [R202606g-SEC-4])")
	}
}
