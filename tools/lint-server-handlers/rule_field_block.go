// rule 3a: text-level field-block godoc marker check for wshub*.go.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// scanFieldBlockMarkers (rule 3a) returns one Violation per wshub_*.go file
// missing the godoc field-block marker. Text-level check, not AST: it
// catches the common case of adding a method and forgetting the header.
//
// Markers: "Field-block contract:" (wshub.go, which owns the Hub struct and
// its block table); "WRITES:" (a wshub_<block>.go method file);
// "READS-ALSO:" (cross-block read-only access); "LIFECYCLE-METHOD"
// (Shutdown / Start / NewHub cross-block write exemption). Presence only;
// marker content is not validated.
func scanFieldBlockMarkers(serverPkg string) []Violation {
	var out []Violation

	entries, err := os.ReadDir(serverPkg)
	if err != nil {
		return out
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		name := e.Name()
		// Only wshub*.go inside internal/server hold the Hub implementation.
		if !strings.HasPrefix(name, "wshub") {
			continue
		}

		path := filepath.Join(serverPkg, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		header := extractGodocHeader(string(data))

		// wshub.go: must contain "Field-block contract"
		if name == "wshub.go" {
			if !strings.Contains(header, "Field-block contract") {
				out = append(out, Violation{
					Rule:    "field_block",
					File:    filepath.ToSlash(path),
					Message: "wshub.go missing 'Field-block contract:' godoc header (server-split-phase4-design.md v0.6.1 §五)",
				})
			}
			continue
		}

		// hub_<block>.go: must contain WRITES / READS-ALSO / LIFECYCLE-METHOD
		if hasAnyMarker(header, "WRITES:", "READS-ALSO:", "LIFECYCLE-METHOD") {
			continue
		}
		out = append(out, Violation{
			Rule:    "field_block",
			File:    filepath.ToSlash(path),
			Message: fmt.Sprintf("%s missing field-block godoc marker (WRITES: / READS-ALSO: / LIFECYCLE-METHOD); rule 3a Phase 0b expects markers ahead of Phase 4b strict-mode rule 3b AST 对账 (§五)", name),
		})
	}
	return out
}

// extractGodocHeader returns the first ~200 lines of a Go source file up to
// the first func declaration, so both file-level and type-level godoc
// count: rule 3a is about declaring intent, not where the comment attaches.
func extractGodocHeader(text string) string {
	lines := strings.SplitN(text, "\n", 250)
	var sb strings.Builder
	for i, line := range lines {
		if i >= 200 {
			break
		}
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "func ") {
			break
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return sb.String()
}

func hasAnyMarker(text string, markers ...string) bool {
	for _, m := range markers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}
