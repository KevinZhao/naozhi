package main

import (
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
	vs, err := scanHandleDecl(dir, []string{"Server.listed", "Server.gone"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 {
		t.Fatalf("got %d violations, want 2: %+v", len(vs), vs)
	}
	got := vs[0]
	if got.Rule != "handle_decl" || got.File != filepath.ToSlash(filepath.Join(dir, "h.go")) || got.Line != 8 || !strings.HasPrefix(got.Message, "unlisted is an HTTP handler") {
		t.Errorf("unlisted handler reported as %+v, want handle_decl at h.go:8", got)
	}
	stale := vs[1]
	if stale.Rule != "handle_decl" || stale.File != defaultExemptionsPath || !strings.Contains(stale.Message, `"Server.gone" matches no HTTP handler`) {
		t.Errorf("stale entry reported as %+v", stale)
	}
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
	vs, err := scanHandleDecl(filepath.Join("..", "..", "internal", "server"), ex.HandleBaseline)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		t.Errorf("%s:%d: %s", v.File, v.Line, v.Message)
	}
}
