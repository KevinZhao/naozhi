package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeServerPkg writes src as server.go, plus a HubOptions / sendEngineOpts
// fixture sized exactly at their baselines and fully read (optsFixtureSrc),
// so tests that only exercise Server's struct_budget / server_field_liveness
// do not also have to satisfy the two other pinned structs' checks.
func writeServerPkg(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opts_fixture.go"), []byte(optsFixtureSrc(hubOptionsFieldBaseline, sendEngineOptsFieldBaseline)), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// optsFixtureSrc defines HubOptions and sendEngineOpts with hubN / sendN
// fields respectively, each read once by a function taking that type by
// value — the shape struct_budget and option_liveness expect.
func optsFixtureSrc(hubN, sendN int) string {
	var b strings.Builder
	b.WriteString("package server\n\n")
	write := func(typeName string, n int) {
		fmt.Fprintf(&b, "type %s struct {\n", typeName)
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "\tF%d int\n", i)
		}
		b.WriteString("}\n\n")
		fmt.Fprintf(&b, "func use%s(o %s) int {\n\treturn ", typeName, typeName)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(" + ")
			}
			fmt.Fprintf(&b, "o.F%d", i)
		}
		if n == 0 {
			b.WriteString("0")
		}
		b.WriteString("\n}\n\n")
	}
	write("HubOptions", hubN)
	write("sendEngineOpts", sendN)
	return b.String()
}

// A field read only by build steps is reported; one read by a method or by a
// function taking *Server is live; an assignment is not a read.
func TestScanServerFields_Liveness(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("package server\n\ntype Server struct {\n\tlive1 int\n\tlive2 int\n\tbuildOnly int\n\twrittenOnly int\n")
	for i := 4; i < serverFieldBaseline; i++ {
		b.WriteString("\tpad" + string(rune('a'+i)) + " int\n")
	}
	b.WriteString("}\n\nfunc buildServer(s *Server) { _ = s.buildOnly }\n")
	b.WriteString("func (s *Server) Start() { _ = s.live1; s.writtenOnly = 1 }\n")
	b.WriteString("func helper(srv *Server) int { return srv.live2 }\n")
	for i := 4; i < serverFieldBaseline; i++ {
		b.WriteString("func (s *Server) use" + string(rune('a'+i)) + "() { _ = s.pad" + string(rune('a'+i)) + " }\n")
	}
	vs := scanServerFields(writeServerPkg(t, b.String()))
	var dead []string
	for _, v := range vs {
		if v.Rule == "server_field_liveness" {
			dead = append(dead, strings.Fields(v.Message)[0])
		}
		if v.Rule == "struct_budget" {
			t.Errorf("unexpected budget violation: %s", v.Message)
		}
	}
	if len(dead) != 2 || dead[0] != "Server.buildOnly" || dead[1] != "Server.writtenOnly" {
		t.Errorf("dead fields = %v, want [Server.buildOnly Server.writtenOnly]", dead)
	}
}

// The field count is held in both directions.
func TestScanServerFields_Budget(t *testing.T) {
	t.Parallel()
	count := func(n int) []Violation {
		var b strings.Builder
		b.WriteString("package server\n\ntype Server struct {\n")
		for i := 0; i < n; i++ {
			b.WriteString("\tf" + strings.Repeat("x", i+1) + " int\n")
		}
		b.WriteString("}\n\nfunc (s *Server) use() {\n")
		for i := 0; i < n; i++ {
			b.WriteString("\t_ = s.f" + strings.Repeat("x", i+1) + "\n")
		}
		b.WriteString("}\n")
		var out []Violation
		for _, v := range scanServerFields(writeServerPkg(t, b.String())) {
			if v.Rule == "struct_budget" {
				out = append(out, v)
			}
		}
		return out
	}
	if vs := count(serverFieldBaseline); len(vs) != 0 {
		t.Errorf("at the baseline: %+v", vs)
	}
	if vs := count(serverFieldBaseline + 1); len(vs) != 1 || !strings.Contains(vs[0].Message, "above the baseline") {
		t.Errorf("one over: %+v", vs)
	}
	if vs := count(serverFieldBaseline - 1); len(vs) != 1 || !strings.Contains(vs[0].Message, "lower serverFieldBaseline") {
		t.Errorf("one under: %+v", vs)
	}
}

// struct_budget also pins HubOptions and sendEngineOpts (#2897 S5a), each
// held in both directions independently: growing one and shrinking the other
// in the same change must report both, not cancel out.
func TestScanServerFields_AdditionalBudgets(t *testing.T) {
	t.Parallel()
	dir := func(hubN, sendN int) string {
		d := t.TempDir()
		server := writeAtBaselineServerSrc()
		if err := os.WriteFile(filepath.Join(d, "server.go"), []byte(server), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "opts_fixture.go"), []byte(optsFixtureSrc(hubN, sendN)), 0o600); err != nil {
			t.Fatal(err)
		}
		return d
	}
	optsBudgetViolations := func(d string) []Violation {
		var out []Violation
		for _, v := range scanServerFields(d) {
			if v.Rule == "struct_budget" && !strings.HasPrefix(v.Message, "Server ") {
				out = append(out, v)
			}
		}
		return out
	}
	if vs := optsBudgetViolations(dir(hubOptionsFieldBaseline, sendEngineOptsFieldBaseline)); len(vs) != 0 {
		t.Errorf("at the baseline: %+v", vs)
	}
	if vs := optsBudgetViolations(dir(hubOptionsFieldBaseline+1, sendEngineOptsFieldBaseline)); len(vs) != 1 ||
		!strings.Contains(vs[0].Message, "HubOptions has") || !strings.Contains(vs[0].Message, "above the baseline") {
		t.Errorf("hub one over: %+v", vs)
	}
	if vs := optsBudgetViolations(dir(hubOptionsFieldBaseline, sendEngineOptsFieldBaseline-1)); len(vs) != 1 ||
		!strings.Contains(vs[0].Message, "lower sendEngineOptsFieldBaseline") {
		t.Errorf("send one under: %+v", vs)
	}
	// Both directions at once: a raise on one struct does not cancel a drop
	// on the other.
	if vs := optsBudgetViolations(dir(hubOptionsFieldBaseline+1, sendEngineOptsFieldBaseline-1)); len(vs) != 2 {
		t.Errorf("both directions at once: %+v", vs)
	}
}

// writeAtBaselineServerSrc is a Server struct at serverFieldBaseline, every
// field read once, so TestScanServerFields_AdditionalBudgets's fixtures do
// not also trip Server's own struct_budget / server_field_liveness.
func writeAtBaselineServerSrc() string {
	var b strings.Builder
	b.WriteString("package server\n\ntype Server struct {\n")
	for i := 0; i < serverFieldBaseline; i++ {
		fmt.Fprintf(&b, "\tf%d int\n", i)
	}
	b.WriteString("}\n\nfunc (s *Server) use() {\n")
	for i := 0; i < serverFieldBaseline; i++ {
		fmt.Fprintf(&b, "\t_ = s.f%d\n", i)
	}
	b.WriteString("}\n")
	return b.String()
}
