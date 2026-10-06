package claudefs

// workflow.go — where a Workflow tool run lives on disk (RFC
// docs/rfc/workflow-dashboard.md §8.1): its run directory
// <projectsRoot>/<slug>/<sid>/subagents/workflows/<runId> with the agents'
// transcripts, and its result file <slug>/<sid>/workflows/<runId>.json.
// Every source of a run's location is untrusted (stream frames,
// sessions.json), so one function vets them all.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/naozhi/naozhi/internal/osutil"
)

// WorkflowRunSource is what a run directory is resolved from, in order: the
// launch receipt's transcript dir, then each project dir with SessionID.
type WorkflowRunSource struct {
	TranscriptDir string
	// ProjectDirs are <projectsRoot>/<slug> candidates for the session's
	// workspace, realpath spelling first (WorkspaceProjectDirs).
	ProjectDirs []string
	// SessionID is the session the run hangs under; "" skips ProjectDirs.
	SessionID string
	RunID     string
}

// WorkflowRun is a vetted run directory. RunDir is spelled with the
// projects root's own bytes; Rel and ResultRel are relative to that root,
// for opening through an os.Root; SessionID is the <sid> Rel names.
type WorkflowRun struct {
	RunDir, Rel, ResultRel, SessionID string
}

// ResolveWorkflowRunDir returns src's run directory when one source names
// an existing directory (not a symlink) that resolves strictly inside
// projectsRoot (already symlink-resolved, ResolvedProjectsRoot) with the
// shape <slug>/<sid>/subagents/workflows/<RunID>. It does I/O.
func ResolveWorkflowRunDir(projectsRoot string, src WorkflowRunSource) (WorkflowRun, bool) {
	if projectsRoot == "" || !IsValidWorkflowRunID(src.RunID) {
		return WorkflowRun{}, false
	}
	if src.TranscriptDir != "" {
		if run, ok := vetRunDir(projectsRoot, src.TranscriptDir, src.RunID); ok {
			return run, true
		}
	}
	if !IsValidSessionID(src.SessionID) {
		return WorkflowRun{}, false
	}
	for _, pd := range src.ProjectDirs {
		if run, ok := vetRunDir(projectsRoot, WorkflowRunDir(SubagentsDir(pd, src.SessionID), src.RunID), src.RunID); ok {
			return run, true
		}
	}
	return WorkflowRun{}, false
}

// vetRunDir checks one candidate run directory.
func vetRunDir(root, candidate, runID string) (WorkflowRun, bool) {
	if candidate == "" || !filepath.IsAbs(candidate) {
		return WorkflowRun{}, false
	}
	if fi, err := os.Lstat(candidate); err != nil || !fi.IsDir() {
		return WorkflowRun{}, false
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return WorkflowRun{}, false
	}
	rel, ok := osutil.RelUnderRoot(resolved, root)
	if !ok {
		return WorkflowRun{}, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 5 || !isSlug(parts[0]) || !IsValidSessionID(parts[1]) ||
		parts[2] != "subagents" || parts[3] != workflowsDirName || parts[4] != runID {
		return WorkflowRun{}, false
	}
	rel = filepath.Join(parts...)
	return WorkflowRun{
		RunDir:    filepath.Join(root, rel),
		Rel:       rel,
		ResultRel: filepath.Join(parts[0], parts[1], workflowsDirName, runID+".json"),
		SessionID: parts[1],
	}, true
}

// isSlug reports whether s can be a ProjectSlug: [A-Za-z0-9-], non-empty.
func isSlug(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isASCIIAlnum(s[i]) && s[i] != '-' {
			return false
		}
	}
	return true
}

// WorkspaceProjectDirs are the project dirs a CLI started in workspace may
// write under: CC slugs the realpath of its cwd, a session's workspace may
// be spelled through a symlink (/tmp → /private/tmp), so the realpath's
// comes first and the spelling as given second when it differs. It does I/O.
func WorkspaceProjectDirs(projectsRoot, workspace string) []string {
	if projectsRoot == "" || workspace == "" {
		return nil
	}
	given := filepath.Join(projectsRoot, ProjectSlug(workspace))
	if real, err := filepath.EvalSymlinks(workspace); err == nil {
		if r := filepath.Join(projectsRoot, ProjectSlug(real)); r != given {
			return []string{r, given}
		}
	}
	return []string{given}
}

// maxWorkflowRuns bounds the run directories LocateWorkflowRun lists in one
// session; maxLocateAgents the agent ids it probes in each.
const (
	maxWorkflowRuns = 256
	maxLocateAgents = 4
)

// ErrTooManyWorkflowRuns: LocateWorkflowRun gave up on a session holding
// more than maxWorkflowRuns run directories.
var ErrTooManyWorkflowRuns = errors.New("claudefs: too many workflow runs to scan")

// LocateWorkflowRun finds the run directory of sessionID, under one of
// projectDirs, that holds a transcript of one of agentIDs (CC's agent ids
// are random, so one match names the run), and returns its run ID; "" when
// none does. Everything is opened through an os.Root at projectsRoot, the
// listing is bounded, and a symlink or special file is never followed.
func LocateWorkflowRun(projectsRoot string, projectDirs []string, sessionID string, agentIDs []string) (string, error) {
	var ids []string
	for _, id := range agentIDs {
		if isAgentID(id) && len(ids) < maxLocateAgents {
			ids = append(ids, id)
		}
	}
	if projectsRoot == "" || !IsValidSessionID(sessionID) || len(ids) == 0 {
		return "", nil
	}
	root, err := os.OpenRoot(projectsRoot)
	if err != nil {
		return "", err
	}
	defer root.Close()
	for _, pd := range projectDirs {
		slug := filepath.Base(pd)
		if filepath.Dir(pd) != projectsRoot || !isSlug(slug) {
			continue
		}
		runs := filepath.Join(slug, sessionID, "subagents", workflowsDirName)
		d, err := osutil.OpenDirIn(root, runs)
		if err != nil {
			continue
		}
		ents, _ := d.ReadDir(maxWorkflowRuns + 1)
		d.Close()
		if len(ents) > maxWorkflowRuns {
			return "", ErrTooManyWorkflowRuns
		}
		for _, e := range ents {
			if !e.IsDir() || !IsValidWorkflowRunID(e.Name()) {
				continue
			}
			for _, id := range ids {
				fi, err := root.Lstat(filepath.Join(runs, e.Name(), "agent-"+id+".jsonl"))
				if err == nil && fi.Mode().IsRegular() {
					return e.Name(), nil
				}
			}
		}
	}
	return "", nil
}

// isAgentID reports whether s has the shape of a CC agent id
// ([A-Za-z0-9]{8,64}), which becomes part of a file name.
func isAgentID(s string) bool {
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isASCIIAlnum(s[i]) {
			return false
		}
	}
	return true
}
