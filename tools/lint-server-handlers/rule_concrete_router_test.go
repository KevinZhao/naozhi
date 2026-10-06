package main

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const concreteRouterRuntimeSrc = `package server

import (
	"context"

	"github.com/naozhi/naozhi/internal/session"
)

// A comment naming *session.Router is not a reference.
type holder struct {
	router *session.Router
}

func use(ctx context.Context, r session.Router) {}

var _ = (*session.Router)(nil)
`

const concreteRouterAliasSrc = `package server

import sess "github.com/naozhi/naozhi/internal/session"

func probe(r *sess.Router) {}
`

const concreteRouterWiringSrc = `package server

import "github.com/naozhi/naozhi/internal/session"

func wire(r *session.Router) {}
`

// Every session.Router reference in a runtime file is reported with its line,
// through an import alias too; the wiring files, test files, other packages'
// Router types and comments are not.
func TestScanConcreteRouter(t *testing.T) {
	t.Parallel()
	dir := writeSublockPkg(t, map[string]string{
		"runtime.go":                 concreteRouterRuntimeSrc,
		"alias.go":                   concreteRouterAliasSrc,
		"other.go":                   "package server\n\nimport \"github.com/naozhi/naozhi/internal/dispatch\"\n\nvar _ *dispatch.Router\n",
		"server_options.go":          concreteRouterWiringSrc,
		"handler_set.go":             concreteRouterWiringSrc,
		"build_server.go":            concreteRouterWiringSrc,
		"dispatch_router_adapter.go": concreteRouterWiringSrc,
		"runtime_test.go":            concreteRouterWiringSrc,
	})
	vs := scanConcreteRouter(dir)
	var got []string
	for _, v := range vs {
		if v.Rule != "concrete_router" {
			t.Errorf("rule = %q", v.Rule)
		}
		got = append(got, filepath.Base(v.File)+":"+strconv.Itoa(v.Line))
	}
	want := "alias.go:5,runtime.go:11,runtime.go:14,runtime.go:16"
	if strings.Join(got, ",") != want {
		t.Errorf("reported %v, want %s", got, want)
	}
}

// collectViolations runs the rule over the server package.
func TestCollectViolations_RunsConcreteRouter(t *testing.T) {
	t.Parallel()
	root := writeSublockPkg(t, map[string]string{"server/health.go": concreteRouterAliasSrc})
	vs, err := collectViolations(filepath.Join(root, "server"), filepath.Join(root, "dashboard"), &exemptions{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, v := range vs {
		if v.Rule == "concrete_router" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("concrete_router violations = %d, want 1: %+v", n, vs)
	}
}

// The real server package names the concrete router only in its wiring files.
func TestScanConcreteRouter_ServerPackageClean(t *testing.T) {
	t.Parallel()
	if vs := scanConcreteRouter("../../internal/server"); len(vs) != 0 {
		t.Errorf("internal/server runtime files name *session.Router: %+v", vs)
	}
}
