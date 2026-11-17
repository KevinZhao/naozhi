package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
)

// The remember block (§15.12 ⑧, D106): the gateway writes what the user
// asked the Planner to keep into a workspace's Claude auto-memory, one file
// per memory with frontmatter plus a line in MEMORY.md, and can take it back.
// Only a turn the user started runs it (planner/action), which is what keeps
// outside content out of the memory (D41).

// GlobalMemory is the remember block's workspace for the Planner's own
// memory (its working directory's project), read by no other session.
const GlobalMemory = "global"

// memoryDescRunes caps the description (the text's first line) in the
// frontmatter and the index line.
const memoryDescRunes = 120

var memoryNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Errors Forget reports; the HTTP layer maps them to statuses.
var (
	ErrNoMemory     = errors.New("没有这条记忆（可能已经撤回，或没有记下）")
	ErrNotPlanners  = errors.New("这条记忆不是 Planner 记的，不能在这里撤回")
	ErrMemoryName   = errors.New("name 只能用小写字母、数字、- 和 _，以字母或数字开头，不超过 64 个")
	ErrNoMemoryRoot = errors.New("没有 Claude 目录，记不了")
)

// ValidMemoryName reports whether name may name a memory file.
func ValidMemoryName(name string) bool { return memoryNameRE.MatchString(name) }

// memoryDir is where workspace's memories live: the Planner's own project
// for GlobalMemory, else the registered workspace's directory on this node.
func (a *Agent) memoryDir(workspace string) (string, error) {
	if a.cfg.ClaudeDir == "" {
		return "", ErrNoMemoryRoot
	}
	dir := a.cfg.Layout.PlannerWorkDir()
	if !strings.EqualFold(strings.TrimSpace(workspace), GlobalMemory) {
		if a.cfg.Workspaces == nil {
			return "", errors.New("没有工作区登记表")
		}
		w, d, ok := a.cfg.Workspaces.Find(workspace)
		switch {
		case !ok:
			return "", fmt.Errorf("找不到工作区「%s」（填 ID、不重名的名字，或 global）", workspace)
		case d == "":
			return "", fmt.Errorf("工作区「%s」在这台机器上没有目录", w.Name)
		}
		dir = d
	}
	md := claudefs.MemoryDir(claudefs.ProjectDir(a.cfg.ClaudeDir, dir))
	if md == "" {
		return "", ErrNoMemoryRoot
	}
	return md, nil
}

// Remember writes text as memory name of workspace, replacing the one the
// gateway wrote under that name before, and lists it in MEMORY.md. It
// returns the file's path.
func (a *Agent) Remember(workspace, name, text string) (string, error) {
	if !ValidMemoryName(name) {
		return "", ErrMemoryName
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("remember 缺少 text")
	}
	dir, err := a.memoryDir(workspace)
	if err != nil {
		return "", err
	}
	defer claudefs.LockMemory()()
	path := filepath.Join(dir, name+".md")
	if data, err := os.ReadFile(path); err == nil && !claudefs.PlannerWrittenMemory(data) {
		return "", fmt.Errorf("那里已经有一条同名的记忆（%s.md，不是 Planner 记的），换一个 name", name)
	}
	desc := textutil.TruncateRunes(oneLine(strings.SplitN(text, "\n", 2)[0]), memoryDescRunes)
	descJSON, _ := json.Marshal(desc) // a JSON string is a YAML double-quoted scalar
	body := fmt.Sprintf("---\nname: %s\ndescription: %s\nmetadata:\n  type: project\n  source: %s\n  remembered_at: %s\n---\n\n%s\n",
		name, descJSON, claudefs.MemorySourcePlanner, a.cfg.Now().Format(time.RFC3339), text)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("建不了记忆目录：%w", err)
	}
	if err := osutil.WriteFileAtomic(path, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("写不了记忆文件：%w", err)
	}
	if _, err := claudefs.EditMemoryIndex(filepath.Join(dir, claudefs.MemoryIndexFile), name, "- ["+name+"]("+name+".md) — "+desc); err != nil {
		return path, fmt.Errorf("记忆写下了，索引没有更新：%w", err)
	}
	slog.Info("planner: remembered", "path", path)
	return path, nil
}

// Forget takes back memory name of workspace the gateway wrote: the file and
// its line in MEMORY.md.
func (a *Agent) Forget(workspace, name string) error {
	if !ValidMemoryName(name) {
		return ErrMemoryName
	}
	dir, err := a.memoryDir(workspace)
	if err != nil {
		return err
	}
	defer claudefs.LockMemory()()
	path := filepath.Join(dir, name+".md")
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return ErrNoMemory
	case err != nil:
		return err
	case !claudefs.PlannerWrittenMemory(data):
		return ErrNotPlanners
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if _, err := claudefs.EditMemoryIndex(filepath.Join(dir, claudefs.MemoryIndexFile), name, ""); err != nil {
		return fmt.Errorf("记忆文件删了，索引行没有删掉：%w", err)
	}
	slog.Info("planner: memory taken back", "path", path)
	return nil
}

// MemoryHome is what the settings view's 懂你 section reads from, so it lists
// the memories this agent writes: the Claude directory (the remember block's
// root) and the Planner's working directory, whose project is the Planner's
// own memory (GlobalMemory). A nil Agent has neither.
func (a *Agent) MemoryHome() (claudeDir, plannerDir string) {
	if a == nil {
		return "", ""
	}
	return a.cfg.ClaudeDir, a.cfg.Layout.PlannerWorkDir()
}
