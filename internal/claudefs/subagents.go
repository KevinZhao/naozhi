package claudefs

import (
	"os"
	"path/filepath"
	"strings"
)

// maxSubagentFiles bounds the entries listed in one sub-agents or workflow
// run directory.
const maxSubagentFiles = 4096

// SubagentTranscripts are the transcripts of sessionID's sub-agents under
// projectDir: the Agent tool's (<subagents>/agent-<id>.jsonl) and each
// workflow run's (<subagents>/workflows/<runId>/agent-<id>.jsonl). Only
// regular files are listed and no symlink is followed, at either level; the
// listings are bounded. It does I/O.
func SubagentTranscripts(projectDir, sessionID string) []string {
	if !IsValidSessionID(sessionID) {
		return nil
	}
	sub := SubagentsDir(projectDir, sessionID)
	if sub == "" {
		return nil
	}
	out := agentTranscriptsIn(sub, maxSubagentFiles)
	runs := WorkflowRunsDir(sub)
	ents := listDir(runs, maxWorkflowRuns)
	for _, e := range ents {
		if e.IsDir() && IsValidWorkflowRunID(e.Name()) {
			out = append(out, agentTranscriptsIn(filepath.Join(runs, e.Name()), maxSubagentFiles)...)
		}
	}
	return out
}

// agentTranscriptsIn are the agent-<id>.jsonl regular files directly in dir.
func agentTranscriptsIn(dir string, limit int) []string {
	var out []string
	for _, e := range listDir(dir, limit) {
		name := e.Name()
		id, ok := strings.CutPrefix(name, "agent-")
		if !ok || !e.Type().IsRegular() {
			continue
		}
		if id, ok = strings.CutSuffix(id, ".jsonl"); ok && isAgentID(id) {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out
}

// listDir lists up to limit entries of dir, nil when dir is not a directory
// (a symlink to one included).
func listDir(dir string, limit int) []os.DirEntry {
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer f.Close()
	ents, _ := f.ReadDir(limit)
	return ents
}
