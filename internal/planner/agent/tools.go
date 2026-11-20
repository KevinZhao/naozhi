package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/planner/reader"
	"github.com/naozhi/naozhi/internal/planner/scout"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// Tools is the Planner's built-in tool set: the file reads and CC's own
// cross-session channel. Bash for gh's reads (ghRead) and the MCP servers'
// read tools come on top (scope, docs/rfc/planner.md D129).
var Tools = []string{"Read", "Grep", "Glob", "ListAgents", "SendMessage"}

// mcpServers are the MCP servers the Planner reads through, by their names
// in cli.mcp_config: the scout's and the reader's.
var mcpServers = []string{scout.ServerOutlook, scout.ServerSlack, scout.ServerAsana,
	reader.ServerExa, reader.ServerAgentCore, reader.ServerBuilder}

// moreRead are read tools the Planner has beyond the scout's and the
// reader's, by the names the servers list (tools/list, checked 2026-11-20):
// what a question asked in conversation needs and a patrol does not.
var moreRead = map[string][]string{
	scout.ServerOutlook:  {"calendar_availability"},
	scout.ServerSlack:    {"get_canvas_content"},
	scout.ServerAsana:    {"asana___AsanaSearch", "asana___GetProject", "asana___GetTasksFromProject", "asana___GetSubtasksForTask"},
	reader.ServerBuilder: {"InternalSearch", "InternalCodeSearch", "TicketingReadActions", "SearchAcronymCentral"},
}

// ghRead are the Bash commands the Planner may run: gh's reads of pull
// requests, issues, runs and releases, so it tells a PR's or a run's state
// from GitHub rather than from a report. `gh api` is left out: it writes.
// The CLI splits a compound command and checks each part, so a rule does
// not carry what is chained after it, and it holds the Read deny rules
// against the file commands it lets through unasked (cat, ls, head; checked
// on 2026-11-20 with the Planner's own settings file).
var ghRead = []string{
	"Bash(gh pr view:*)", "Bash(gh pr list:*)", "Bash(gh pr checks:*)", "Bash(gh pr diff:*)", "Bash(gh pr status:*)",
	"Bash(gh issue view:*)", "Bash(gh issue list:*)",
	"Bash(gh run list:*)", "Bash(gh run view:*)",
	"Bash(gh release list:*)", "Bash(gh release view:*)",
	"Bash(gh repo view:*)", "Bash(gh search:*)",
}

// mcpRead and mcpWrite are the Planner's read tools of servers, and the
// write tools denied by name, as the CLI names them, in server order.
func mcpRead(servers []string) (read, write []string) {
	for _, s := range servers {
		for _, t := range slices.Concat(scout.ReadTools(s), reader.Tools(s), moreRead[s]) {
			read = append(read, scout.MCPTool(s, t))
		}
		for _, t := range scout.WriteTools(s) {
			write = append(write, scout.MCPTool(s, t))
		}
	}
	return read, write
}

// scope is what planner:main's process starts with beyond its fixed spec:
// its tools, its permission rules and its MCP servers. All of it rides in
// the spawn options; a live process started on another is restarted on its
// conversation once idle (refreshScope).
type scope struct {
	tools, allow, deny []string
	// mcpPath is the --mcp-config written for it, "" with no server; mcpSum
	// is that file's content hash, so a rotated key restarts the process too.
	mcpPath, mcpSum string
}

// sum fingerprints s for agent.json (persisted.Scope).
func (s scope) sum() string {
	h := sha256.New()
	for _, part := range [][]string{s.tools, s.allow, s.deny, {s.mcpPath, s.mcpSum}} {
		for _, v := range part {
			h.Write([]byte(v))
			h.Write([]byte{0})
		}
		h.Write([]byte{1})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// apply puts s into opts.
func (s scope) apply(opts *sessionview.AgentOpts) {
	opts.Tools, opts.PermissionsAllow, opts.PermissionsDeny, opts.MCPConfig = s.tools, s.allow, s.deny, s.mcpPath
}

// scope reads the Planner's scope as it is now: the read rules over the
// registered workspaces, and the read tools of the servers cli.mcp_config
// defines, whose cut-down copy it writes when it changed. Without servers
// (no cli.mcp_config, or none of mcpServers in it) the Planner has its
// built-in tools only; Bash comes with the naozhi-owned settings file alone
// (Config.OwnSettings).
func (a *Agent) scope() scope {
	allow, deny := a.readRules()
	s := scope{tools: slices.Clone(Tools), allow: allow, deny: deny}
	if a.cfg.OwnSettings {
		s.tools = append(s.tools, "Bash")
		s.allow = append(s.allow, ghRead...)
	}
	if !a.cfg.Enabled || a.cfg.MCPConfig == "" {
		return s
	}
	data, servers, err := scout.CutMCPConfig(a.cfg.MCPConfig, mcpServers)
	if err != nil {
		// Every reconcile comes here: not worth a warning each minute.
		slog.Debug("planner: MCP servers unavailable; built-in tools only", "err", err)
		return s
	}
	path := a.cfg.Layout.PlannerMCPConfigPath()
	if old, err := os.ReadFile(path); err != nil || !bytes.Equal(old, data) {
		if err := datadir.EnsureDir(filepath.Dir(path)); err != nil {
			slog.Warn("planner: MCP config directory", "err", err)
			return s
		}
		if err := osutil.WriteFileAtomic(path, data, 0o600); err != nil {
			slog.Warn("planner: write MCP config", "path", path, "err", err)
			return s
		}
	}
	read, write := mcpRead(servers)
	sum := sha256.Sum256(data)
	s.tools = append(s.tools, read...)
	s.allow = append(s.allow, read...)
	s.deny = append(s.deny, write...)
	s.mcpPath, s.mcpSum = path, hex.EncodeToString(sum[:])
	return s
}
