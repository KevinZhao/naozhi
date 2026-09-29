package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSublockPkg(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const sublockSrc = `package p

import "sync"

type reg struct{ mu sync.RWMutex }

func (r *reg) own() { r.mu.Lock(); r.mu.Unlock() }

type hub struct {
	tailers *reg
	shards  []reg
	mu      sync.Mutex
}

func (h *hub) self() { h.mu.Lock(); defer h.mu.Unlock() }
func (h *hub) shard(i int) { h.shards[i].mu.Lock(); h.shards[i].mu.Unlock() }
`

// A reach-through call in production code is reported with its line; a
// lock on the receiver's own field, or on an indexed shard it owns, is not.
func TestScanSublocks_Production(t *testing.T) {
	t.Parallel()
	dir := writeSublockPkg(t, map[string]string{
		"p.go":     sublockSrc,
		"sub/q.go": "package sub\n\nfunc f(h *struct{ t *struct{ mu interface{ RLock() } } }) {\n\th.t.mu.RLock()\n}\n",
	})
	vs := scanSublocks([]string{dir}, 0)
	if len(vs) != 1 {
		t.Fatalf("got %d violations, want 1: %+v", len(vs), vs)
	}
	if v := vs[0]; !strings.HasSuffix(v.File, "sub/q.go") || v.Line != 4 || v.Rule != "sublock_encapsulation" {
		t.Errorf("violation = %+v, want sub/q.go:4", v)
	}
}

// Test files are counted against the baseline in both directions.
func TestScanSublocks_TestBaseline(t *testing.T) {
	t.Parallel()
	test := "package p\n\nfunc g(h *hub) {\n\th.tailers.mu.Lock()\n\tdefer h.tailers.mu.Unlock()\n\th.tailers.mu.TryRLock()\n}\n"
	dir := writeSublockPkg(t, map[string]string{"p.go": sublockSrc, "p_test.go": test})
	if vs := scanSublocks([]string{dir}, 3); len(vs) != 0 {
		t.Errorf("at the baseline: %+v", vs)
	}
	if vs := scanSublocks([]string{dir}, 2); len(vs) != 1 || !strings.Contains(vs[0].Message, "above the baseline of 2") {
		t.Errorf("above the baseline: %+v", vs)
	}
	if vs := scanSublocks([]string{dir}, 4); len(vs) != 1 || !strings.Contains(vs[0].Message, "lower sublockTestBaseline to 3") {
		t.Errorf("below the baseline: %+v", vs)
	}
}

func TestReachThroughLock_OnlyLockMethods(t *testing.T) {
	t.Parallel()
	src := "package p\n\nfunc g(h *hub) { h.tailers.mu.Snapshot(); h.tailers.lookup() }\n"
	dir := writeSublockPkg(t, map[string]string{"p.go": sublockSrc, "g.go": src})
	if vs := scanSublocks([]string{dir}, 0); len(vs) != 0 {
		t.Errorf("a non-lock method was reported: %+v", vs)
	}
}

// collectViolations runs the rule over the server package it is given.
func TestCollectViolations_RunsSublock(t *testing.T) {
	t.Parallel()
	dir := writeSublockPkg(t, map[string]string{
		"server.go": "package server\n\nfunc f(h *struct{ t *struct{ mu interface{ Lock() } } }) { h.t.mu.Lock() }\n",
	})
	vs, err := collectViolations(dir, filepath.Join(dir, "absent"), &exemptions{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Rule == "sublock_encapsulation" && strings.Contains(v.Message, "another object's lock") {
			return
		}
	}
	t.Errorf("no reach-through violation among %+v", vs)
}
