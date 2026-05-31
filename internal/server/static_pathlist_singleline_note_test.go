package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_FencedPathList_SingleLineAndNote pins the fix for two blind
// spots that left fenced path blocks button-less even though every line was a
// real file path:
//
//	Blind spot A — single-line fence. The old guard `paths.length < 2` demoted
//	a lone path to verbatim <pre><code>, which scanEventForFileRefs skips. AI
//	routinely wraps ONE generated file in a ``` fence, so single-file fences
//	never got preview/download buttons.
//
//	Blind spot B — trailing annotations. Lines like `foo.md   ← 待生成` carry a
//	human note set off by whitespace. isFileRefCandidate rejects whitespace, so
//	the whole block failed the candidate test and fell back to verbatim render.
//
// As with the sibling fence/link contract tests we pin the JS source (no JS
// runner in CI) by windowing on the helper and its render branch.
func TestDashboardJS_FencedPathList_SingleLineAndNote(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// --- splitPathNote helper (blind spot B) ---
	noteIdx := strings.Index(js, "function splitPathNote(line)")
	if noteIdx < 0 {
		t.Fatal("splitPathNote helper not found — the trailing-annotation fix (blind spot B) is missing")
	}
	noteRest := js[noteIdx:]
	noteEnd := strings.Index(noteRest[1:], "\nfunction ")
	if noteEnd < 0 {
		noteEnd = len(noteRest)
	}
	noteBody := noteRest[:noteEnd]
	if !strings.Contains(noteBody, "note") {
		t.Error("splitPathNote must return a {path, note} split so trailing annotations are preserved for display but excluded from the path")
	}

	// --- fencedPathList wiring ---
	fnIdx := strings.Index(js, "function fencedPathList(code)")
	if fnIdx < 0 {
		t.Fatal("fencedPathList helper not found")
	}
	rest := js[fnIdx:]
	end := strings.Index(rest[1:], "\nfunction ")
	if end < 0 {
		end = len(rest)
	}
	body := rest[:end]

	// Blind spot B: the candidate test must run on the note-stripped path, not
	// the raw line — otherwise a trailing `← 待生成` re-breaks isFileRefCandidate.
	if !strings.Contains(body, "splitPathNote(line)") {
		t.Error("fencedPathList must call splitPathNote(line) before isFileRefCandidate so trailing annotations don't fail the candidate test (blind spot B)")
	}
	// Blind spot A: a single path line must be accepted.
	if strings.Contains(body, "paths.length < 2") {
		t.Error("fencedPathList must accept a single-line path fence (blind spot A) — the `paths.length < 2` guard regresses it")
	}

	// --- render branch must surface the note without polluting the <code> ---
	renderIdx := strings.Index(js, "md-code-wrap md-pathlist")
	if renderIdx < 0 {
		t.Fatal("path-list render branch not found")
	}
	// Window back to the start of the rows map so we can assert both the
	// <code>-holds-path and note-outside-code contract.
	mapIdx := strings.LastIndex(js[:renderIdx], "pathLines.map(")
	if mapIdx < 0 {
		t.Fatal("pathLines.map render expression not found")
	}
	seg := js[mapIdx:renderIdx]
	if !strings.Contains(seg, "p.path") {
		t.Error("render branch must put p.path inside the row <code> so the file-ref scanner + copy see the bare path")
	}
	if !strings.Contains(seg, "md-pathnote") {
		t.Error("render branch must emit a .md-pathnote span for p.note so the annotation is visible but kept outside the <code> body")
	}
	// The note span must be a sibling of <code>, not nested inside it. The
	// emitted row concatenates the closed <code> followed by the note span, so
	// pin the `'</code>' + noteHtml` shape (note appended AFTER the code closes).
	if !strings.Contains(seg, "'</code>' + noteHtml") {
		t.Error("the row must append the note span AFTER '</code>' (as a sibling) so the note is never folded into the path the exists-check queries")
	}
}

// TestDashboardHTML_PathnoteStyles pins the CSS for the trailing annotation so
// it renders dimmed next to the path rather than looking like part of it.
func TestDashboardHTML_PathnoteStyles(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)
	if !strings.Contains(html, ".md-pathnote{") {
		t.Error("dashboard.html must define .md-pathnote styles for the fenced path-line trailing annotation")
	}
}
