package project

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// sweepLegacyStubs removes, once per root, the .naozhi/project.yaml files
// that earlier Scans wrote only to persist CreatedAt. A file counts as such a
// stub only when it is the sole entry of .naozhi/ and its bytes equal
// yaml.Marshal(ProjectConfig{CreatedAt: X}) for the X the index holds, and
// only once that index is on disk. Callers hold scanMu; the sweep takes
// m.mu.Lock itself so a writer's save cannot land between compare and remove.
func (m *Manager) sweepLegacyStubs() {
	if !m.index.stubCleanupPending(m.root) || !m.index.durable() {
		return
	}
	m.mu.Lock()
	for _, p := range m.projects {
		if ms := m.index.createdAt[p.Path]; !p.IsRoot && ms != 0 {
			removeLegacyStub(p.Name, p.Path, ms)
		}
	}
	m.mu.Unlock()
	m.index.markStubCleanupDone(m.root)
}

// removeLegacyStub deletes projDir's .naozhi/project.yaml, and the
// then-empty .naozhi/, when the file is a byte-exact Scan stub for createdAt
// in a real (non-symlink) .naozhi/; anything else is left untouched.
func removeLegacyStub(name, projDir string, createdAt int64) {
	dir := filepath.Join(projDir, configDir)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != configFile {
		return
	}
	want, err := yaml.Marshal(&ProjectConfig{CreatedAt: createdAt})
	if err != nil {
		return
	}
	path := filepath.Join(dir, configFile)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(want)) {
		return
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		return
	}
	if err := os.Remove(path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("remove legacy project.yaml stub failed", "project", name, "path", path, "err", err)
		}
		return
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Debug("keep .naozhi dir after stub removal", "project", name, "path", dir, "err", err)
	}
	slog.Info("removed legacy project.yaml stub; sidebar order kept in the projects index",
		"project", name, "path", path, "created_at", createdAt)
}
