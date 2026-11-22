package patrol

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/planner/github"
	"github.com/naozhi/naozhi/internal/planner/workspace"
)

const (
	// maxStatusPaths caps the uncommitted paths whose times are read.
	maxStatusPaths = 2000
	// branchRunes caps a branch name in an item's text.
	branchRunes = 50
)

// dirReport is what the checks found in one directory of a workspace.
type dirReport struct {
	dir string
	// notRepo says dir is in no git repository: only the worktree check
	// applies to it.
	notRepo bool
	// err is the local git failing: the directory's section keeps its last
	// body, and its dirty and branch items stay as they are. repoUnknown
	// says it failed before telling which repository dir is in: the file's
	// GitHub sections are kept as well.
	err         error
	repoUnknown bool
	dirty       []hit
	// branches are the branches pushed and left; branchErr is that check
	// not made (the pull requests could not be asked for), notes what it
	// left out.
	branches  []hit
	branchErr error
	notes     []string
	worktrees []hit
	// gh is the GitHub repository dir is in, nil for none.
	gh *repoReport
}

// inspect makes the git checks in dir, and the GitHub ones of the
// repository it is in unless another directory already did this round.
func (p *Patrol) inspect(ctx context.Context, rd *round, t target, dir string) dirReport {
	rep := dirReport{dir: dir, worktrees: p.leftWorktrees(dir)}
	out, err := p.call(ctx, dir, "git", "rev-parse", "--show-toplevel", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		// Exec keeps git's messages in English (LC_ALL=C).
		if strings.Contains(err.Error(), "not a git repository") {
			rep.notRepo = true
		} else {
			rep.err, rep.repoUnknown = err, true
		}
		return rep
	}
	lines := strings.Split(strings.TrimRight(out, "\r\n"), "\n")
	if len(lines) != 3 {
		rep.err, rep.repoUnknown = errors.New("git rev-parse: 输出不认识"), true
		return rep
	}
	top, gitDir, common := lines[0], lines[1], lines[2]
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	// The repository first: its section is in the file whatever becomes of
	// the local checks below.
	remotes, err := p.call(ctx, dir, "git", "remote", "-v")
	if err != nil {
		rep.err, rep.repoUnknown = err, true
		return rep
	}
	if owner, name, ok := github.PickRemote(remotes); ok {
		rep.gh = p.github(ctx, rd, t.ID, owner, name, dir)
	}
	if rep.dirty, rep.err = p.dirty(ctx, dir, top, rd.now); rep.err != nil {
		return rep
	}
	// A linked worktree's git directory is one below the repository's.
	p.branches(ctx, rd, &rep, sameFile(gitDir, common))
	return rep
}

func sameFile(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ai, err1 := os.Stat(a)
	bi, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(ai, bi)
}

// dirty is the changes below dir not committed, when the one untouched the
// longest was last written dirtyDays ago or more. The time is the file's:
// git keeps none for a change not committed. A change that is a deletion
// has no time and only counts.
func (p *Patrol) dirty(ctx context.Context, dir, top string, now time.Time) ([]hit, error) {
	out, err := p.call(ctx, dir, "git", "status", "--porcelain=v1", "-z", "--", ".")
	if err != nil {
		return nil, err
	}
	paths := statusPaths(out)
	var oldest time.Time
	for _, rel := range paths[:min(len(paths), maxStatusPaths)] {
		fi, err := os.Lstat(filepath.Join(top, rel))
		if err != nil {
			continue
		}
		if oldest.IsZero() || fi.ModTime().Before(oldest) {
			oldest = fi.ModTime()
		}
	}
	if oldest.IsZero() || now.Sub(oldest) < dirtyDays*day {
		return nil, nil
	}
	text := fmt.Sprintf("%d 处改动没提交，最早的在 %d 天前", len(paths), days(now, oldest))
	return []hit{{Source: sourcePrefix + checkDirty + ":" + dir, Text: text, Line: text}}, nil
}

// statusPaths is the paths `git status --porcelain=v1 -z` lists, relative
// to the repository's top. An untracked .DS_Store is left out: macOS drops
// one in every folder Finder opened, and it would keep every repository
// that does not ignore it hitting for good.
func statusPaths(out string) []string {
	var paths []string
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		xy, path := f[:2], f[3:]
		if strings.ContainsAny(xy, "RC") {
			i++ // a rename's or a copy's original path follows
		}
		if xy == "??" && filepath.Base(path) == ".DS_Store" {
			continue
		}
		paths = append(paths, path)
	}
	return paths
}

// branch is a local branch not merged into the default branch.
type branch struct {
	name string
	// remote is the name it was pushed to origin under, "" for not pushed:
	// its own, else that of the origin branch it tracks.
	remote string
	tip    time.Time
	// worktree is where it is checked out, "" for nowhere.
	worktree string
}

// pushedAs is the name a local branch is on origin under, "" when it was
// not pushed: its own name when origin has it, else the origin branch it
// tracks when its tip is in that one (it was pushed under another name).
// An upstream alone does not say pushed: a branch cut from a remote branch
// tracks it from its first commit, and a branch may track a local one. The
// pull request, if any, is under the name returned.
func pushedAs(name, upstream, track, def string, remote map[string]bool) string {
	if remote[name] {
		return name
	}
	up, ok := strings.CutPrefix(upstream, "refs/remotes/origin/")
	if !ok || !remote[up] || up == def || strings.Contains(track, "ahead") {
		return ""
	}
	return up
}

// branches is the local branches pushed (pushedAs) whose last commit is
// branchDays old and that are not merged into the default branch. In a
// GitHub repository merged is having, or having had, a pull request: a
// squash merge leaves the branch no ancestor of anything. Elsewhere it is
// being an ancestor of the remote default branch as last fetched; the
// patrol fetches nothing.
//
// Only branches made here are looked at, and each by one directory: the
// one it is checked out in when a registered directory holds that
// worktree, else the repository's main worktree. Several worktrees of one
// repository registered as workspaces would otherwise each file every
// branch.
func (p *Patrol) branches(ctx context.Context, rd *round, rep *dirReport, main bool) {
	dir := rep.dir
	out, err := p.call(ctx, dir, "git", "for-each-ref", "--format=%(refname)", "refs/remotes/origin")
	if err != nil {
		rep.err = err
		return
	}
	remote := map[string]bool{}
	for line := range strings.SplitSeq(out, "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "refs/remotes/origin/"); ok && name != "HEAD" {
			remote[name] = true
		}
	}
	// origin/HEAD is only set by a clone; a repository that added its
	// remote later has none.
	def := ""
	if out, err := p.call(ctx, dir, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		def, _ = strings.CutPrefix(strings.TrimSpace(out), "origin/")
	}
	if !remote[def] {
		def = ""
		for _, n := range []string{"main", "master"} {
			if remote[n] {
				def = n
				break
			}
		}
	}
	if len(remote) == 0 {
		return // nothing was pushed to origin
	}
	if def == "" {
		rep.notes = append(rep.notes, "找不到远端的默认分支（origin/HEAD、origin/main、origin/master 都没有），没查推上去的分支")
		return
	}
	out, err = p.call(ctx, dir, "git", "for-each-ref", "--no-merged=refs/remotes/origin/"+def,
		"--format=%(refname)%00%(upstream)%00%(upstream:track)%00%(committerdate:unix)%00%(worktreepath)", "refs/heads")
	if err != nil {
		rep.err = err
		return
	}
	var left []branch
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Split(line, "\x00")
		if len(f) != 5 {
			continue
		}
		name, ok := strings.CutPrefix(f[0], "refs/heads/")
		unix, err := strconv.ParseInt(f[3], 10, 64)
		if !ok || err != nil {
			continue
		}
		b := branch{name: name, remote: pushedAs(name, f[1], f[2], def, remote), tip: time.Unix(unix, 0), worktree: f[4]}
		if b.name == def || b.remote == "" || rd.now.Sub(b.tip) < branchDays*day || !reports(rd.dirs, dir, main, b.worktree) {
			continue
		}
		left = append(left, b)
	}
	slices.SortFunc(left, func(a, b branch) int {
		return cmp.Or(a.tip.Compare(b.tip), strings.Compare(a.name, b.name))
	})
	if len(left) == 0 {
		return
	}
	if rep.gh == nil {
		for _, b := range left {
			h := branchHit(b, "还没合进 "+def, rd.now)
			h.Line += "（按本地上次 fetch 的状态）"
			rep.branches = append(rep.branches, h)
		}
		return
	}
	if rep.gh.err != nil {
		rep.branchErr = rep.gh.err
		return
	}
	unknown := 0
	for _, b := range left {
		has, known, err := p.hasPull(ctx, rep.gh, dir, b.remote)
		switch {
		case err != nil:
			rep.branches, rep.branchErr = nil, err
			return
		case !known:
			unknown++
		case !has:
			rep.branches = append(rep.branches, branchHit(b, "还没开 PR", rd.now))
		}
	}
	if unknown > 0 {
		rep.notes = append(rep.notes, fmt.Sprintf("另有 %d 个分支这一轮没查有没有开过 PR（一轮最多查 %d 个）", unknown, maxHeadLookups))
	}
}

func branchHit(b branch, what string, now time.Time) hit {
	as := " "
	if b.remote != b.name {
		as = "（远端是 " + clip(fold(b.remote), branchRunes) + "）"
	}
	text := fmt.Sprintf("分支 %s%s推上去了%s，最后的提交在 %d 天前", clip(fold(b.name), branchRunes), as, what, days(now, b.tip))
	return hit{Source: sourcePrefix + checkBranch + ":" + b.name, Text: text, Line: text}
}

// reports tells whether dir is the directory that reports a branch checked
// out at worktree ("" for nowhere): the deepest registered directory that
// holds the worktree, else the main worktree.
func reports(dirs []workspace.Dir, dir string, main bool, worktree string) bool {
	if worktree != "" {
		holder := ""
		for _, d := range dirs {
			if p := filepath.Clean(d.Path); within(worktree, p) && len(p) > len(holder) {
				holder = p
			}
		}
		if holder != "" {
			return holder == filepath.Clean(dir)
		}
	}
	return main
}

// within reports whether path is root or lies below it. Not by the bytes
// alone: the registry keeps a directory as the session came in by, git
// says what is on disk, and on a filesystem that folds case the two may
// spell one directory differently.
func within(path, root string) bool {
	return osutil.PathContainedInRoot(path, root)
}

// leftWorktrees is the worktrees a put-away left whose repository is in
// dir and that are still on disk.
func (p *Patrol) leftWorktrees(dir string) []hit {
	if p.cfg.Worktrees == nil {
		return nil
	}
	var hits []hit
	for _, wt := range p.cfg.Worktrees() {
		if wt.Dir == "" || wt.GitDir == "" || !within(filepath.Dir(filepath.Clean(wt.GitDir)), filepath.Clean(dir)) {
			continue
		}
		source := sourcePrefix + checkWorktree + ":" + wt.Dir
		if slices.ContainsFunc(hits, func(h hit) bool { return h.Source == source }) {
			continue
		}
		if fi, err := os.Stat(wt.Dir); err != nil || !fi.IsDir() {
			continue
		}
		name := wt.Dir
		if rel, ok := osutil.RelUnderRoot(filepath.Clean(wt.Dir), filepath.Clean(dir)); ok {
			name = rel
		}
		text := "收起时留下的 worktree " + fold(name)
		if why := fold(wt.Why); why != "" {
			text += "（" + why + "）"
		}
		hits = append(hits, hit{Source: source, Text: text, Line: text})
	}
	slices.SortFunc(hits, func(a, b hit) int { return strings.Compare(a.Source, b.Source) })
	return hits
}

// days is the whole days from t to now.
func days(now, t time.Time) int { return int(now.Sub(t) / day) }

// fold is s on one line.
func fold(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip is s cut to n runes, the last an ellipsis when it was cut.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
