package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const lateSetterSrc = `package p

type T struct{}

func (t *T) SetOnChange(fn func())                        {}
func (t *T) SetDiscoverFunc(fn func() error)              {}
func (t *T) SetTelemetry(b interface{ Emit() })           {}
func (t *T) SetWidgetHook(fn func(string) bool)           {}
func (t *T) SetFilter(fn func(string) bool)               {}
func (t *T) SetBackend(id string)                         {}
func (t *T) SetBounded(k string, n int, live func() bool) {}
func (t *T) SetWatch(fn func(), n int)                    {}
func (t *T) SetPair(a, b func())                          {}
func (t *T) setOnChange(fn func())                        {}
func SetOnChange(fn func())                               {}
`

// Each late-injection shape is reported with its line; data setters,
// multi-argument calls, unexported methods and plain functions are not.
func TestScanLateSetters(t *testing.T) {
	t.Parallel()
	dir := writeSublockPkg(t, map[string]string{
		"p.go":      lateSetterSrc,
		"p_test.go": "package p\n\nfunc (t *T) SetOnTest(fn func()) {}\n",
	})
	vs := scanLateSetters([]string{dir})
	var got []string
	for _, v := range vs {
		if v.Rule != "no_late_setters" {
			t.Errorf("rule = %q", v.Rule)
		}
		got = append(got, v.Message[:strings.Index(v.Message, " ")])
	}
	want := "SetOnChange,SetDiscoverFunc,SetTelemetry,SetWidgetHook,SetFilter"
	if strings.Join(got, ",") != want {
		t.Errorf("reported %v, want %s", got, want)
	}
	if len(vs) > 0 && vs[0].Line != 5 {
		t.Errorf("first violation at line %d, want 5", vs[0].Line)
	}
}

// collectViolations scans the core packages next to the server package, and
// skips any that are absent.
func TestCollectViolations_RunsLateSetters(t *testing.T) {
	t.Parallel()
	root := writeSublockPkg(t, map[string]string{
		"server/server.go": "package server\n",
		"cron/c.go":        "package cron\n\ntype S struct{}\n\nfunc (s *S) SetOnTick(fn func()) {}\n",
	})
	vs, err := collectViolations(filepath.Join(root, "server"), filepath.Join(root, "dashboard"), &exemptions{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, v := range vs {
		if v.Rule == "no_late_setters" {
			n++
			if !strings.HasSuffix(v.File, "cron/c.go") {
				t.Errorf("violation in %s", v.File)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d no_late_setters violations, want 1: %+v", n, vs)
	}
}
