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
// yaml.Marshal(ProjectConfig{CreatedAt: X}), and only once the index holding
// X (yaml wins, so Scan just put it there) is on disk. Callers hold m.mu.Lock.
func (m *Manager) sweepLegacyStubs(projects map[string]*Project) {
	if !m.index.stubCleanupPending(m.root) || !m.index.durable() {
		return
	}
	for _, p := range projects {
		if p.IsRoot || p.Config.CreatedAt == 0 {
			continue
		}
		removeLegacyStub(p)
	}
	m.index.markStubCleanupDone(m.root)
}

// removeLegacyStub deletes p's project.yaml, and the then-empty .naozhi/,
// when the file is a byte-exact Scan stub in a real (non-symlink) .naozhi/;
// anything else is left untouched.
func removeLegacyStub(p *Project) {
	dir := filepath.Join(p.Path, configDir)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != configFile {
		return
	}
	want, err := yaml.Marshal(&ProjectConfig{CreatedAt: p.Config.CreatedAt})
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
			slog.Warn("remove legacy project.yaml stub failed", "project", p.Name, "path", path, "err", err)
		}
		return
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Debug("keep .naozhi dir after stub removal", "project", p.Name, "path", dir, "err", err)
	}
	slog.Info("removed legacy project.yaml stub; sidebar order kept in the projects index",
		"project", p.Name, "path", path, "created_at", p.Config.CreatedAt)
}
