package agent

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ruleCovers reports whether a Read(...) rule's glob matches path, home
// standing for "~": "**" any run of directories, "*" one name's part.
func ruleCovers(rule, home, path string) bool {
	g, ok := strings.CutPrefix(rule, "Read(")
	if !ok {
		return false
	}
	g = strings.TrimSuffix(g, ")")
	switch {
	case strings.HasPrefix(g, "~/"):
		g = home + g[1:]
	case strings.HasPrefix(g, "//"):
		g = g[1:]
	}
	var re strings.Builder
	re.WriteString("^")
	for i := 0; i < len(g); i++ {
		switch {
		case strings.HasPrefix(g[i:], "/**"):
			re.WriteString("(/.*)?")
			i += 2
		case strings.HasPrefix(g[i:], "**"):
			re.WriteString(".*")
			i++
		case g[i] == '*':
			re.WriteString("[^/]*")
		default:
			re.WriteString(regexp.QuoteMeta(g[i : i+1]))
		}
	}
	re.WriteString("$")
	return regexp.MustCompile(re.String()).MatchString(path)
}

func covered(rules []string, home, path string) bool {
	for _, r := range rules {
		if ruleCovers(r, home, path) {
			return true
		}
	}
	return false
}

// TestReadScopeGitHub (§15.14 ⑤): the GitHub issue files the gateway writes
// under planner/github are in the Planner's read scope and no deny rule
// covers them.
func TestReadScopeGitHub(t *testing.T) {
	now := time.Now()
	a, lay := newTestAgent(t, &fakeSessions{}, &now)
	o := a.Opts()
	file := filepath.Join(lay.PlannerGitHubRoot(), "KevinZhao-naozhi.md")
	if !covered(o.PermissionsAllow, a.cfg.Home, file) {
		t.Errorf("%s is outside the read scope: %q", file, o.PermissionsAllow)
	}
	if covered(o.PermissionsDeny, a.cfg.Home, file) {
		t.Errorf("%s is denied: %q", file, o.PermissionsDeny)
	}
	// The matcher does see a deny rule's file.
	if yaml := filepath.Join(lay.PlannerGitHubRoot(), "x.yaml"); !covered(o.PermissionsDeny, a.cfg.Home, yaml) {
		t.Errorf("matcher misses the deny rule for %s", yaml)
	}
}

// TestReadScopeClaudeProjects pins why the Planner's Grep over
// ~/.claude/projects was refused on 11-15 (§15.14 ⑤): only the memory
// directories under it are in scope, not the tree itself, whose session
// transcripts carry the pages the taint deny rules keep out of the
// Planner's context. A search must name a memory directory.
func TestReadScopeClaudeProjects(t *testing.T) {
	now := time.Now()
	a, _ := newTestAgent(t, &fakeSessions{}, &now)
	allow, home := a.Opts().PermissionsAllow, a.cfg.Home
	mem := filepath.Join(home, ".claude", "projects", "-Users-u-w-x", "memory", "MEMORY.md")
	if !covered(allow, home, mem) {
		t.Errorf("memory index out of scope: %q", allow)
	}
	for _, p := range []string{
		filepath.Join(home, ".claude", "projects"),
		filepath.Join(home, ".claude", "projects", "-Users-u-w-x", "0f1e.jsonl"),
	} {
		if covered(allow, home, p) {
			t.Errorf("%s in the read scope: transcripts would bypass the taint deny rules", p)
		}
	}
}
