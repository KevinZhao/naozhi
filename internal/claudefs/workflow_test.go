package claudefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	wfTestSID   = "04a8fc10-6fa5-4b8e-82ba-621974425917"
	wfTestRun   = "wf_2997921d-435"
	wfTestAgent = "a2093755b9a9ce8c0"
)

// wfLayout is a projects root holding one session's workflow run.
type wfLayout struct {
	root, slug, projectDir, runDir string
}

func newWFLayout(t *testing.T) wfLayout {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := wfLayout{root: filepath.Join(tmp, "projects"), slug: "-home-u-ws"}
	l.projectDir = filepath.Join(l.root, l.slug)
	l.runDir = WorkflowRunDir(SubagentsDir(l.projectDir, wfTestSID), wfTestRun)
	if err := os.MkdirAll(l.runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.runDir, "agent-"+wfTestAgent+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return l
}

func (l wfLayout) want() WorkflowRun {
	rel := filepath.Join(l.slug, wfTestSID, "subagents", "workflows", wfTestRun)
	return WorkflowRun{
		RunDir:    filepath.Join(l.root, rel),
		Rel:       rel,
		ResultRel: filepath.Join(l.slug, wfTestSID, "workflows", wfTestRun+".json"),
		SessionID: wfTestSID,
	}
}

// TestResolveWorkflowRunDir_Accepts: a real run dir resolves from either
// source, with the relative paths an os.Root opens and the <sid> its path
// names. Pins the containment check's argument order: swapped, every legal
// run dir is refused.
func TestResolveWorkflowRunDir_Accepts(t *testing.T) {
	t.Parallel()
	l := newWFLayout(t)
	for name, src := range map[string]WorkflowRunSource{
		"transcript dir": {TranscriptDir: l.runDir, RunID: wfTestRun},
		"project dir":    {ProjectDirs: []string{filepath.Join(l.root, "-absent"), l.projectDir}, SessionID: wfTestSID, RunID: wfTestRun},
		"bad first":      {TranscriptDir: "/nowhere", ProjectDirs: []string{l.projectDir}, SessionID: wfTestSID, RunID: wfTestRun},
	} {
		run, ok := ResolveWorkflowRunDir(l.root, src)
		if !ok || run != l.want() {
			t.Errorf("%s: got %+v, %v; want %+v", name, run, ok, l.want())
		}
	}
}

// TestResolveWorkflowRunDir_Refuses: invalid IDs, paths outside the root or
// above it, the root itself, a wrong shape, a mismatched run id, and a run
// dir that is a symlink are all refused; no session id skips the project
// dirs.
func TestResolveWorkflowRunDir_Refuses(t *testing.T) {
	t.Parallel()
	l := newWFLayout(t)
	outside := filepath.Join(filepath.Dir(l.root), "elsewhere", wfTestSID, "subagents", "workflows", wfTestRun)
	wrongShape := filepath.Join(l.projectDir, wfTestSID, "workflows", wfTestRun)
	wrongMiddle := filepath.Join(l.projectDir, wfTestSID, "transcripts", "workflows", wfTestRun)
	for _, d := range []string{outside, wrongShape, wrongMiddle} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A run dir of another session that is a symlink to this one's: it
	// resolves to a well-shaped dir with the same run id.
	other := WorkflowRunDir(SubagentsDir(l.projectDir, "11111111-1111-4111-8111-111111111111"), wfTestRun)
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(l.runDir, other); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	cases := map[string]WorkflowRunSource{
		"run id with ..":     {TranscriptDir: l.runDir, RunID: "wf_../x"},
		"run id bare prefix": {TranscriptDir: l.runDir, RunID: "wf_"},
		"run id too long":    {TranscriptDir: l.runDir, RunID: "wf_" + strings.Repeat("a", 65)},
		"run id mismatch":    {TranscriptDir: l.runDir, RunID: "wf_2997921d-436"},
		"outside the root":   {TranscriptDir: outside, RunID: wfTestRun},
		"an ancestor":        {TranscriptDir: "/", RunID: wfTestRun},
		"the root":           {TranscriptDir: l.root, RunID: wfTestRun},
		"wrong shape":        {TranscriptDir: wrongShape, RunID: wfTestRun},
		"wrong middle":       {TranscriptDir: wrongMiddle, RunID: wfTestRun},
		"relative":           {TranscriptDir: filepath.Join(l.slug, wfTestSID, "subagents", "workflows", wfTestRun), RunID: wfTestRun},
		"symlinked run dir":  {TranscriptDir: other, RunID: wfTestRun},
		"no session id":      {ProjectDirs: []string{l.projectDir}, RunID: wfTestRun},
		"bad session id":     {ProjectDirs: []string{l.projectDir}, SessionID: "x/../" + wfTestSID, RunID: wfTestRun},
		"project outside":    {ProjectDirs: []string{filepath.Join(filepath.Dir(l.root), "elsewhere")}, SessionID: wfTestSID, RunID: wfTestRun},
	}
	for name, src := range cases {
		if run, ok := ResolveWorkflowRunDir(l.root, src); ok {
			t.Errorf("%s: resolved %+v", name, run)
		}
	}
	if _, ok := ResolveWorkflowRunDir("", WorkflowRunSource{TranscriptDir: l.runDir, RunID: wfTestRun}); ok {
		t.Error("resolved with no projects root")
	}
}

// TestResolveWorkflowRunDir_CaseVariantRoot: on a case-insensitive
// filesystem a transcript dir whose root part differs in case is accepted
// through the inode branch, and RunDir is respelled in the root's case so
// byte-prefix gates downstream accept it too.
func TestResolveWorkflowRunDir_CaseVariantRoot(t *testing.T) {
	t.Parallel()
	l := newWFLayout(t)
	upper := filepath.Join(filepath.Dir(l.root), "PROJECTS")
	if _, err := os.Stat(upper); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	src := WorkflowRunSource{TranscriptDir: strings.Replace(l.runDir, l.root, upper, 1), RunID: wfTestRun}
	run, ok := ResolveWorkflowRunDir(l.root, src)
	if !ok || run != l.want() {
		t.Errorf("got %+v, %v; want %+v spelled under the root", run, ok, l.want())
	}
}

// TestResolveWorkflowRunDir_SymlinkWorkspace: CC slugs its cwd's realpath.
// A workspace spelled through a symlink still finds the run from the
// project dirs alone, realpath first.
func TestResolveWorkflowRunDir_SymlinkWorkspace(t *testing.T) {
	t.Parallel()
	l := newWFLayout(t)
	realWS := filepath.Join(filepath.Dir(l.root), "real", "ws")
	if err := os.MkdirAll(realWS, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(l.root), "link")
	if err := os.Symlink(filepath.Join(filepath.Dir(l.root), "real"), link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	slug := ProjectSlug(realWS)
	runDir := WorkflowRunDir(SubagentsDir(filepath.Join(l.root, slug), wfTestSID), wfTestRun)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dirs := WorkspaceProjectDirs(l.root, filepath.Join(link, "ws"))
	want := []string{filepath.Join(l.root, slug), filepath.Join(l.root, ProjectSlug(filepath.Join(link, "ws")))}
	if fmt.Sprint(dirs) != fmt.Sprint(want) {
		t.Fatalf("WorkspaceProjectDirs = %v, want %v", dirs, want)
	}
	run, ok := ResolveWorkflowRunDir(l.root, WorkflowRunSource{ProjectDirs: dirs, SessionID: wfTestSID, RunID: wfTestRun})
	if !ok || run.RunDir != runDir {
		t.Errorf("got %+v, %v; want %s", run, ok, runDir)
	}
	if got := WorkspaceProjectDirs(l.root, realWS); len(got) != 1 {
		t.Errorf("a workspace without a symlink yields %v, want one dir", got)
	}
	if WorkspaceProjectDirs("", realWS) != nil || WorkspaceProjectDirs(l.root, "") != nil {
		t.Error("project dirs without a root or workspace")
	}
}

// TestWorkflowPathHelpers: the joins return "" for an invalid input, so a
// caller that skips ResolveWorkflowRunDir still cannot build a path out.
func TestWorkflowPathHelpers(t *testing.T) {
	t.Parallel()
	sub := filepath.Join("p", "s", "subagents")
	if got := WorkflowRunsDir(sub); got != filepath.Join(sub, "workflows") {
		t.Errorf("WorkflowRunsDir = %q", got)
	}
	if got := WorkflowRunDir(sub, wfTestRun); got != filepath.Join(sub, "workflows", wfTestRun) {
		t.Errorf("WorkflowRunDir = %q", got)
	}
	if got := WorkflowResultFile("p", wfTestSID, wfTestRun); got != filepath.Join("p", wfTestSID, "workflows", wfTestRun+".json") {
		t.Errorf("WorkflowResultFile = %q", got)
	}
	for name, got := range map[string]string{
		"runs dir, no subagents": WorkflowRunsDir(""),
		"run dir, bad run":       WorkflowRunDir(sub, "../wf_x"),
		"run dir, no subagents":  WorkflowRunDir("", wfTestRun),
		"result, bad run":        WorkflowResultFile("p", wfTestSID, "wf_"),
		"result, bad session":    WorkflowResultFile("p", "..", wfTestRun),
		"result, no project":     WorkflowResultFile("", wfTestSID, wfTestRun),
	} {
		if got != "" {
			t.Errorf("%s = %q, want \"\"", name, got)
		}
	}
}

// TestLocateWorkflowRun: the run holding one of the agents' transcripts is
// found; invalid agent ids are not probed; a symlinked transcript does not
// count; more than 256 runs gives up.
func TestLocateWorkflowRun(t *testing.T) {
	t.Parallel()
	l := newWFLayout(t)
	runs := filepath.Dir(l.runDir)
	if err := os.MkdirAll(filepath.Join(runs, "wf_00000000-000"), 0o755); err != nil {
		t.Fatal(err)
	}
	dirs := []string{l.projectDir}
	got, err := LocateWorkflowRun(l.root, dirs, wfTestSID, []string{"../x", "a0000000000000000", wfTestAgent})
	if err != nil || got != wfTestRun {
		t.Errorf("LocateWorkflowRun = %q, %v; want %s", got, err, wfTestRun)
	}
	if got, _ := LocateWorkflowRun(l.root, dirs, wfTestSID, []string{"a0000000000000000"}); got != "" {
		t.Errorf("an unknown agent located %q", got)
	}
	if got, _ := LocateWorkflowRun(l.root, dirs, "", []string{wfTestAgent}); got != "" {
		t.Errorf("no session id located %q", got)
	}
	link := filepath.Join(runs, "wf_11111111-111", "agent-a1111111111111111.jsonl")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(l.runDir, "agent-"+wfTestAgent+".jsonl"), link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	if got, _ := LocateWorkflowRun(l.root, dirs, wfTestSID, []string{"a1111111111111111"}); got != "" {
		t.Errorf("a symlinked transcript located %q", got)
	}
	for i := range maxWorkflowRuns {
		if err := os.Mkdir(filepath.Join(runs, fmt.Sprintf("wf_%08d-x", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := LocateWorkflowRun(l.root, dirs, wfTestSID, []string{wfTestAgent}); !errors.Is(err, ErrTooManyWorkflowRuns) {
		t.Errorf("over %d runs: err = %v, want ErrTooManyWorkflowRuns", maxWorkflowRuns, err)
	}
}
