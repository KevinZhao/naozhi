package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeServerPkg(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
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
