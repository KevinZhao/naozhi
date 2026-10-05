package project

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// trackState is what git says about a project's .naozhi/project.yaml.
type trackState int

const (
	trackUnknown trackState = iota
	trackUntracked
	trackTracked
)

// stubProbeTimeout bounds one git ls-files run of the stub sweep.
const stubProbeTimeout = 5 * time.Second

// legacyStub names one project whose .naozhi/project.yaml may be a stub.
type legacyStub struct {
	name, dir string
	createdAt int64
}

// sweepLegacyStubs removes, once per root, the .naozhi/project.yaml files
// that earlier Scans wrote only to persist CreatedAt. A file counts as such a
// stub only when it is the sole entry of .naozhi/ and its bytes equal
// yaml.Marshal(ProjectConfig{CreatedAt: X}) for the X the index holds, only
// once that index is on disk, and only when git does not track it. Callers
// hold scanMu. The git probe runs without m.mu; the removal re-checks the
// bytes under m.mu.Lock so a writer's save cannot land between compare and
// remove.
func (m *Manager) sweepLegacyStubs() {
	if !m.index.stubCleanupPending(m.root) || !m.index.durable() {
		return
	}
	var stubs []legacyStub
	m.mu.RLock()
	for _, p := range m.projects {
		if ms := m.index.createdAt[p.Path]; !p.IsRoot && ms != 0 {
			stubs = append(stubs, legacyStub{name: p.Name, dir: p.Path, createdAt: ms})
		}
	}
	m.mu.RUnlock()

	doomed := stubs[:0]
	for _, s := range stubs {
		if s.isStub() && !m.keepTrackedStub(s) {
			doomed = append(doomed, s)
		}
	}
	if len(doomed) > 0 {
		m.mu.Lock()
		for _, s := range doomed {
			s.remove()
		}
		m.mu.Unlock()
	}
	m.index.markStubCleanupDone(m.root)
}

// isStub reports whether s's .naozhi/ is a real (non-symlink) directory whose
// sole entry is a byte-exact Scan stub for s.createdAt.
func (s legacyStub) isStub() bool {
	dir := filepath.Join(s.dir, configDir)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != configFile {
		return false
	}
	want, err := yaml.Marshal(&ProjectConfig{CreatedAt: s.createdAt})
	if err != nil {
		return false
	}
	path := filepath.Join(dir, configFile)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(want)) {
		return false
	}
	got, err := os.ReadFile(path)
	return err == nil && bytes.Equal(got, want)
}

// keepTrackedStub reports whether the sweep must keep s's stub: git tracks
// it, or git cannot tell and s.dir has its own .git.
func (m *Manager) keepTrackedStub(s legacyStub) bool {
	path := filepath.Join(s.dir, configDir, configFile)
	switch m.stubProbe(s.dir) {
	case trackTracked:
		slog.Info("kept legacy project.yaml stub: tracked by git", "project", s.name, "path", path)
		return true
	case trackUntracked:
		return false
	}
	if _, err := os.Lstat(filepath.Join(s.dir, ".git")); err != nil {
		return false
	}
	slog.Info("kept legacy project.yaml stub: git could not tell whether it is tracked",
		"project", s.name, "path", path)
	return true
}

// gitTracksStub asks git whether projDir's .naozhi/project.yaml is in the
// index. Exit 1 means untracked; a missing git, a non-repo, a refused repo
// (exit 128) or a timeout are trackUnknown.
func gitTracksStub(projDir string) trackState {
	ctx, cancel := context.WithTimeout(context.Background(), stubProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", projDir, "-c", "core.fsmonitor=false",
		"ls-files", "--error-unmatch", "--", configDir+"/"+configFile)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return trackTracked
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
		return trackUntracked
	}
	return trackUnknown
}

// remove deletes s's stub, and the then-empty .naozhi/, if it is still a
// byte-exact stub; the caller holds m.mu.Lock.
func (s legacyStub) remove() {
	if !s.isStub() {
		return
	}
	dir := filepath.Join(s.dir, configDir)
	path := filepath.Join(dir, configFile)
	if err := os.Remove(path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("remove legacy project.yaml stub failed", "project", s.name, "path", path, "err", err)
		}
		return
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Debug("keep .naozhi dir after stub removal", "project", s.name, "path", dir, "err", err)
	}
	slog.Info("removed legacy project.yaml stub; sidebar order kept in the projects index",
		"project", s.name, "path", path, "created_at", s.createdAt)
}
