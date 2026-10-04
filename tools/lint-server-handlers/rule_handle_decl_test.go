package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Every shape scanHandlerDecls must recognise, and the look-alikes it must not.
func TestScanHandlerDecls_Shapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"server method under any name", "func (s *Server) serveProbe(w http.ResponseWriter, r *http.Request) {}", []string{"Server.serveProbe"}},
		{"other receiver", "func (h *HealthHandler) handleProbe(w http.ResponseWriter, r *http.Request) {}", []string{"HealthHandler.handleProbe"}},
		{"value receiver", "func (h HealthHandler) probe(w http.ResponseWriter, r *http.Request) {}", []string{"HealthHandler.probe"}},
		{"generic receiver", "func (c *cache[K]) probe(w http.ResponseWriter, r *http.Request) {}", []string{"cache.probe"}},
		{"ServeHTTP", "func (p probeH) ServeHTTP(http.ResponseWriter, *http.Request) {}", []string{"probeH.ServeHTTP"}},
		{"free function", "func probe(w http.ResponseWriter, r *http.Request) {}", []string{"probe"}},
		{"HandlerFunc factory", "func probe(name string) http.HandlerFunc { return nil }", []string{"probe"}},
		{"Handler factory", "func (s *Server) probe() http.Handler { return nil }", []string{"Server.probe"}},
		{"func-type factory", "func probe() func(http.ResponseWriter, *http.Request) { return nil }", []string{"probe"}},
		{"returns bool", "func reject(w http.ResponseWriter, r *http.Request) bool { return false }", nil},
		{"middleware", "func mw(next http.Handler) http.Handler { return next }", nil},
		{"HandlerFunc middleware", "func mw(next http.HandlerFunc) http.HandlerFunc { return next }", nil},
		{"func-type middleware", "func mw(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc { return nil }", nil},
		{"writer helper", "func writeJSON(w http.ResponseWriter, v any) {}", nil},
		{"swapped params", "func probe(r *http.Request, w http.ResponseWriter) {}", nil},
		{"value Request", "func probe(w http.ResponseWriter, r http.Request) {}", nil},
		{"two results", "func probe() (http.Handler, error) { return nil, nil }", nil},
		{"func literal var", "var probe = func(w http.ResponseWriter, r *http.Request) {}", []string{"probe"}},
		{"factory literal var", "var probe = func() http.HandlerFunc { return nil }", []string{"probe"}},
		{"conversion var", "var probe = http.HandlerFunc(nil)", []string{"probe"}},
		{"typed var", "var probe http.Handler", []string{"probe"}},
		{"grouped vars", "var (\n\tprobe, other = func(w http.ResponseWriter, r *http.Request) {}, 1\n)", []string{"probe"}},
		{"blank var", "var _ http.Handler = nil", nil},
		{"non-handler func var", "var probe = func(w http.ResponseWriter) {}", nil},
		{"multi-value var", "var a, b = func() (http.Handler, int) { return nil, 0 }()", nil},
		{"local handler type factory", "type fn func(http.ResponseWriter, *http.Request)\n\nfunc probe() fn { return nil }", []string{"probe"}},
		{"local type chain factory", "type g h\ntype h http.HandlerFunc\n\nfunc probe() g { return nil }", []string{"probe"}},
		{"local type middleware", "type fn func(http.ResponseWriter, *http.Request)\n\nfunc mw(next fn) fn { return next }", nil},
		{"local non-handler type", "type fn func(http.ResponseWriter)\n\nfunc probe() fn { return nil }", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := "package server\n\nimport \"net/http\"\n\nvar _ http.Handler\n\n" + tc.src + "\n"
			dir := writeSublockPkg(t, map[string]string{"p.go": src})
			decls, err := scanHandlerDecls(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := handlerKeys(decls); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("keys = %v, want %v", got, tc.want)
			}
		})
	}
}

// The net/http name comes from the file's own imports, so an alias or a dot
// import is still recognised, and a file without net/http declares nothing.
func TestScanHandlerDecls_ImportNames(t *testing.T) {
	t.Parallel()
	dir := writeSublockPkg(t, map[string]string{
		"alias.go":  "package server\n\nimport nethttp \"net/http\"\n\nfunc a(w nethttp.ResponseWriter, r *nethttp.Request) {}\n",
		"dot.go":    "package server\n\nimport . \"net/http\"\n\nfunc b(w ResponseWriter, r *Request) {}\n",
		"other.go":  "package server\n\nimport http \"example.com/fakehttp\"\n\nfunc c(w http.ResponseWriter, r *http.Request) {}\n",
		"wrong.go":  "package server\n\nimport nethttp \"net/http\"\n\nvar _ nethttp.Handler\n\nfunc d(w http.ResponseWriter, r *http.Request) {}\n",
		"x_test.go": "package server\n\nimport \"net/http\"\n\nfunc e(w http.ResponseWriter, r *http.Request) {}\n",
	})
	decls, err := scanHandlerDecls(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := handlerKeys(decls), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
}

// A listed handler passes; an unlisted one is reported at its declaration; a
// baseline entry naming no declaration is itself a violation.
func TestScanHandleDecl_Baseline(t *testing.T) {
	t.Parallel()
	dir := writeSublockPkg(t, map[string]string{
		"h.go": "package server\n\nimport \"net/http\"\n\n// listed has a doc comment.\nfunc (s *Server) listed(w http.ResponseWriter, r *http.Request) {}\n\nfunc unlisted(w http.ResponseWriter, r *http.Request) {}\n",
	})
	vs, err := scanHandleDecl(dir, []string{"Server.listed", "Server.gone", "Server.listed", "Server.gone"}, "ex.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 4 {
		t.Fatalf("got %d violations, want 4: %+v", len(vs), vs)
	}
	for i, want := range []string{`"Server.listed" more than once`, `"Server.gone" more than once`} {
		if got := vs[i]; got.Rule != "handle_decl" || got.File != "ex.yaml" || !strings.Contains(got.Message, want) {
			t.Errorf("repeated entry reported as %+v, want %s", got, want)
		}
	}
	got := vs[2]
	if got.Rule != "handle_decl" || got.File != filepath.ToSlash(filepath.Join(dir, "h.go")) || got.Line != 8 || !strings.HasPrefix(got.Message, "unlisted is an HTTP handler") {
		t.Errorf("unlisted handler reported as %+v, want handle_decl at h.go:8", got)
	}
	stale := vs[3]
	if stale.Rule != "handle_decl" || stale.File != "ex.yaml" || !strings.Contains(stale.Message, `"Server.gone" matches no HTTP handler`) {
		t.Errorf("stale entry reported as %+v", stale)
	}
}

// A stale entry points at the exemptions file the run actually loaded.
func TestCollectViolations_StaleEntryNamesLoadedFile(t *testing.T) {
	t.Parallel()
	dir := writeSublockPkg(t, map[string]string{"server.go": "package server\n"})
	exPath := filepath.Join(dir, "custom.yaml")
	if err := os.WriteFile(exPath, []byte("handle_baseline:\n  - Server.gone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex, err := loadExemptions(exPath)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := collectViolations(dir, filepath.Join(dir, "absent"), ex, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Rule == "handle_decl" && strings.Contains(v.Message, `"Server.gone"`) {
			if v.File != exPath {
				t.Errorf("stale entry reported at %q, want %q", v.File, exPath)
			}
			return
		}
	}
	t.Errorf("no stale handle_decl violation among %+v", vs)
}

// collectViolations runs the rule over the server package it is given.
func TestCollectViolations_RunsHandleDecl(t *testing.T) {
	t.Parallel()
	dir := writeSublockPkg(t, map[string]string{
		"server.go": "package server\n\nimport \"net/http\"\n\nfunc (h *HealthHandler) handleProbe(w http.ResponseWriter, r *http.Request) {}\n",
	})
	vs, err := collectViolations(dir, filepath.Join(dir, "absent"), &exemptions{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Rule == "handle_decl" && strings.HasPrefix(v.Message, "HealthHandler.handleProbe ") {
			return
		}
	}
	t.Errorf("no handle_decl violation among %+v", vs)
}

// The checked-in baseline is exactly the handlers internal/server declares:
// nothing unlisted, nothing stale.
func TestHandleBaseline_MatchesServerPackage(t *testing.T) {
	t.Parallel()
	ex, err := loadExemptions("exemptions.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.HandleBaseline) == 0 {
		t.Fatal("exemptions.yaml has no handle_baseline entries: the yaml key drifted, this test checks nothing")
	}
	vs, err := scanHandleDecl(filepath.Join("..", "..", "internal", "server"), ex.HandleBaseline, "exemptions.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		t.Errorf("%s:%d: %s", v.File, v.Line, v.Message)
	}
}
