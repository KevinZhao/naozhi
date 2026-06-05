package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFixture builds a self-contained internal/session-like directory with
// the given router_*.go file contents and returns its path. Fixtures are
// fully synthetic so the test does not break when the real Router fields
// change — it exercises the tool's parsing/diff logic, not live annotations.
func writeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func hasViolation(vs []Violation, rule, field, domain string) bool {
	for _, v := range vs {
		if v.Rule == rule && v.Field == field && (domain == "" || v.Domain == domain) {
			return true
		}
	}
	return false
}

// TestCheck_MatchingAnnotationPasses: a field whose `// 读写:` annotation
// declares every domain that actually accesses it produces no violation.
func TestCheck_MatchingAnnotationPasses(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"router_core.go": `package session
type Router struct {
	// 读写: core, lifecycle
	sessions map[string]int
}
func (r *Router) init() { r.sessions = nil }
`,
		"router_lifecycle.go": `package session
func (r *Router) spawn() { _ = r.sessions }
`,
	})

	vs, err := check(dir)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(vs) != 0 {
		t.Errorf("expected 0 violations, got %d: %+v", len(vs), vs)
	}
}

// TestCheck_DriftOmittedFails: a field accessed in a router_*.go domain that
// its annotation omits is reported as drift_omitted.
func TestCheck_DriftOmittedFails(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"router_core.go": `package session
type Router struct {
	// 读写: core
	sessions map[string]int
}
func (r *Router) init() { r.sessions = nil }
`,
		"router_cleanup.go": `package session
func (r *Router) remove() { delete(r.sessions, "k") }
`,
	})

	vs, err := check(dir)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !hasViolation(vs, "drift_omitted", "sessions", "cleanup") {
		t.Errorf("expected drift_omitted on sessions/cleanup, got %+v", vs)
	}
}

// TestCheck_MissingAnnotationReported: a field with no `// 读写:` comment is
// reported as missing_annotation.
func TestCheck_MissingAnnotationReported(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"router_core.go": `package session
type Router struct {
	// plain doc, no read/write marker
	wrapper int
}
func (r *Router) init() { r.wrapper = 0 }
`,
	})

	vs, err := check(dir)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !hasViolation(vs, "missing_annotation", "wrapper", "") {
		t.Errorf("expected missing_annotation on wrapper, got %+v", vs)
	}
}

// TestCheck_WildcardSuppressesDrift: a non-domain leading token (method name
// / "all" coverage phrase) flags the field as wildcard, suppressing
// drift_omitted even when accessed from an undeclared domain.
func TestCheck_WildcardSuppressesDrift(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"router_core.go": `package session
type Router struct {
	// 读写: core, all router_*.go (acquired by methods)
	mu int
}
func (r *Router) init() { r.mu = 0 }
`,
		"router_cleanup.go": `package session
func (r *Router) remove() { _ = r.mu }
`,
	})

	vs, err := check(dir)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if hasViolation(vs, "drift_omitted", "mu", "") {
		t.Errorf("wildcard annotation should suppress drift, got %+v", vs)
	}
}

// TestCheck_IgnoresNonReceiverSelectors: a selector off a non-"r" identifier
// that shares a field name must not count as a Router field access.
func TestCheck_IgnoresNonReceiverSelectors(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"router_core.go": `package session
type Router struct {
	// 读写: core
	sessions map[string]int
}
func (r *Router) init() { r.sessions = nil }
`,
		"router_cleanup.go": `package session
type other struct{ sessions int }
func use(o other) { _ = o.sessions }
`,
	})

	vs, err := check(dir)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if hasViolation(vs, "drift_omitted", "sessions", "cleanup") {
		t.Errorf("o.sessions on unrelated type must not register as drift, got %+v", vs)
	}
}

// TestCheck_SkipsTestFiles: accesses inside router_*_test.go are not scanned.
func TestCheck_SkipsTestFiles(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"router_core.go": `package session
type Router struct {
	// 读写: core
	sessions map[string]int
}
func (r *Router) init() { r.sessions = nil }
`,
		"router_cleanup_test.go": `package session
func testAccess(r *Router) { _ = r.sessions }
`,
	})

	vs, err := check(dir)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(vs) != 0 {
		t.Errorf("test-file access must be ignored, got %+v", vs)
	}
}

// TestParseAnnotation_DomainList verifies comma-segment leading-token parsing
// and wildcard detection directly.
func TestParseAnnotation_DomainList(t *testing.T) {
	fa := fieldAnnotation{domains: map[string]bool{}}
	parseDomainList(&fa, " core (init), lifecycle (spawn/reset)")
	if !fa.domains["core"] || !fa.domains["lifecycle"] {
		t.Errorf("expected core+lifecycle domains, got %+v", fa.domains)
	}
	if fa.wildcard {
		t.Errorf("pure domain list must not flag wildcard")
	}

	fw := fieldAnnotation{domains: map[string]bool{}}
	parseDomainList(&fw, " core, Resolver() (read-only accessor)")
	if !fw.domains["core"] {
		t.Errorf("expected core domain, got %+v", fw.domains)
	}
	if !fw.wildcard {
		t.Errorf("method-name token must flag wildcard")
	}
}

// TestFileDomain verifies router_*.go → domain mapping.
func TestFileDomain(t *testing.T) {
	cases := map[string]string{
		"internal/session/router_capacity.go": "capacity",
		"router_core.go":                      "core",
		"x/router_lifecycle_log.go":           "lifecycle_log",
	}
	for in, want := range cases {
		if got := fileDomain(in); got != want {
			t.Errorf("fileDomain(%q) = %q, want %q", in, got, want)
		}
	}
}
