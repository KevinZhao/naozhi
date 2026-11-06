package reap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Exec is Git over the git and gh binaries.
type Exec struct{}

var _ Git = Exec{}

func run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GH_PROMPT_DISABLED=1", "NO_COLOR=1")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return out.String(), fmt.Errorf("%s %s: %s", name, args[0], msg)
		}
		return out.String(), fmt.Errorf("%s %s: %w", name, args[0], err)
	}
	return out.String(), nil
}

func abs(base, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// Linked implements Git.
func (Exec) Linked(ctx context.Context, dir string) (top, gitDir, branch string, ok bool) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", "", "", false
	}
	out, err := run(ctx, dir, "git", "rev-parse", "--show-toplevel", "--git-dir", "--git-common-dir")
	if err != nil {
		return "", "", "", false
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		return "", "", "", false
	}
	top = filepath.Clean(lines[0])
	own, common := abs(dir, lines[1]), abs(dir, lines[2])
	if own == common {
		return "", "", "", false // the main worktree
	}
	b, err := run(ctx, top, "git", "symbolic-ref", "-q", "--short", "HEAD")
	if err == nil {
		branch = strings.TrimSpace(b)
	}
	return top, common, branch, true
}

// Status implements Git.
func (Exec) Status(ctx context.Context, top string) (Status, error) {
	out, err := run(ctx, top, "git", "status", "--porcelain")
	if err != nil {
		return Status{}, err
	}
	st := Status{Dirty: strings.TrimSpace(out) != ""}
	head, err := run(ctx, top, "git", "rev-parse", "HEAD")
	if err != nil {
		return Status{}, err
	}
	st.Head = strings.TrimSpace(head)
	refs, err := run(ctx, top, "git", "for-each-ref", "--count=1", "--contains", st.Head, "--format=%(refname)", "refs/remotes")
	if err != nil {
		return Status{}, err
	}
	st.Pushed = strings.TrimSpace(refs) != ""
	return st, nil
}

// noPR is gh's answer for a branch with no pull request.
const noPR = "no pull requests found"

// PR implements Git.
func (Exec) PR(ctx context.Context, top, branch string) (PR, error) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return PR{}, errors.New("没有 gh 命令")
	}
	out, err := run(ctx, top, gh, "pr", "view", branch, "--json", "state,headRefOid,url")
	if err != nil {
		if strings.Contains(err.Error(), noPR) {
			return PR{}, nil
		}
		return PR{}, err
	}
	var v struct {
		State      string `json:"state"`
		HeadRefOid string `json:"headRefOid"`
		URL        string `json:"url"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return PR{}, fmt.Errorf("gh pr view: %w", err)
	}
	return PR{State: v.State, Head: v.HeadRefOid, URL: v.URL}, nil
}

// Remove implements Git: without --force, so git refuses a worktree with
// changes even if one appeared after the check.
func (Exec) Remove(ctx context.Context, gitDir, top string) error {
	_, err := run(ctx, filepath.Dir(top), "git", "--git-dir="+gitDir, "worktree", "remove", top)
	return err
}

// Add implements Git.
func (Exec) Add(ctx context.Context, gitDir, top, branch string) error {
	if branch == "" {
		return errors.New("不在分支上")
	}
	if err := os.MkdirAll(filepath.Dir(top), 0o755); err != nil {
		return err
	}
	_, err := run(ctx, filepath.Dir(top), "git", "--git-dir="+gitDir, "worktree", "add", top, branch)
	return err
}
