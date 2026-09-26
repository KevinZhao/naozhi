package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanFieldBlockMarkers_OnlyFilesWithHubMethods(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		// Hub methods, no marker: flagged.
		"wshub_bare.go": "package server\n\nfunc (h *Hub) Bare() {}\n",
		// Hub methods with a marker: fine.
		"wshub_marked.go": "// WRITES: clients\npackage server\n\nfunc (h *Hub) Marked() {}\n",
		// A value receiver still counts as a Hub method.
		"wshub_value.go": "package server\n\nfunc (h Hub) Value() {}\n",
		// A self-owned sub-object: no Hub methods, nothing to declare.
		"wshub_debounce.go": "package server\n\ntype debouncer struct{}\n\nfunc (d *debouncer) trigger() {}\n",
		// Methods on a type whose name merely ends in Hub are not Hub methods.
		"wshub_other.go": "package server\n\ntype fakeHub struct{}\n\nfunc (f *fakeHub) X() {}\n",
		// Not a wshub file at all.
		"send.go": "package server\n\nfunc (h *Hub) Elsewhere() {}\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]bool{}
	for _, v := range scanFieldBlockMarkers(dir) {
		got[filepath.Base(v.File)] = true
	}
	want := map[string]bool{"wshub_bare.go": true, "wshub_value.go": true}
	for name := range want {
		if !got[name] {
			t.Errorf("%s defines Hub methods without a marker but was not flagged", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("%s was flagged but has nothing to declare", name)
		}
	}
}
