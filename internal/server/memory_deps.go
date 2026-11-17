package server

import (
	"path/filepath"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/dashboard/ext/memory"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/planner/workspace"
)

// memoryDeps wires the memory handler: the Planner agent's Claude
// directory when it has one (the one its remember block writes under and
// its read rules cover), else ~/.claude; the registered workspaces, by
// every spelling this node reaches them by, name the 懂你 groups.
func memoryDeps(planner *agent.Agent, reg *workspace.Registry) memory.Deps {
	claudeDir, plannerDir := planner.MemoryHome()
	d := memory.Deps{PlannerDir: plannerDir}
	if claudeDir != "" {
		d.ProjectsDir = claudefs.ProjectsRoot(claudeDir)
		d.ClaudeMD = filepath.Join(claudeDir, "CLAUDE.md")
	} else {
		d.ProjectsDir = resolveClaudeProjectsDir()
		if dir := resolveClaudeDir(); dir != "" {
			d.ClaudeMD = filepath.Join(dir, "CLAUDE.md")
		}
	}
	if reg != nil {
		d.Workspaces = func() []memory.Workspace {
			dirs := reg.LocalDirs()
			out := make([]memory.Workspace, 0, len(dirs))
			byPath := map[string]workspace.Dir{}
			for _, w := range dirs {
				out = append(out, memory.Workspace{ID: w.ID, Name: w.Name, Path: w.Path})
				byPath[w.Path] = w
			}
			for alias, path := range reg.LocalAliases() {
				if w, ok := byPath[path]; ok {
					out = append(out, memory.Workspace{ID: w.ID, Name: w.Name, Path: alias})
				}
			}
			return out
		}
	}
	return d
}
