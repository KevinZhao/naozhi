package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
)

// mcpDoc is a cli.mcp_config with two servers the Planner reads through and
// one it does not know, each with a secret.
func mcpDoc(key string) string {
	return `{"mcpServers":{
"slack-mcp":{"command":"slack-mcp","env":{"SLACK_TOKEN":"FIXTURE-SLACK"}},
"exa":{"type":"http","url":"https://mcp.exa.example/mcp","headers":{"x-api-key":"` + key + `"}},
"atoz-mcp":{"command":"atoz","env":{"ATOZ_TOKEN":"FIXTURE-ATOZ"}}}}`
}

// TestScopeMCP (D129): the Planner gets the read tools of the servers
// cli.mcp_config defines, from a copy cut down to them, allowed by name
// under dontAsk with the write tools denied; a server it does not know
// brings neither its definition nor a tool. A key rotated in cli.mcp_config
// restarts the idle Planner on its conversation, as a new workspace does.
func TestScopeMCP(t *testing.T) {
	now := time.Now()
	lay := datadir.FromRoot(t.TempDir())
	src := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(src, []byte(mcpDoc("FIXTURE-KEY-1")), 0o600); err != nil {
		t.Fatal(err)
	}
	sess := &fakeSessions{st: SessionState{Exists: true, Alive: true, SessionID: "s1"}}
	a := New(Config{Enabled: true, Layout: lay, Home: t.TempDir(), Sessions: sess, MCPConfig: src,
		Now: func() time.Time { return now }})
	a.turns = &fakeTurns{}
	o := a.Opts()
	if o.MCPConfig != lay.PlannerMCPConfigPath() || !o.StrictMCPConfig || o.PermissionMode != "dontAsk" {
		t.Fatalf("opts = %+v", o)
	}
	cut, err := os.ReadFile(o.MCPConfig)
	if err != nil || !strings.Contains(string(cut), "slack-mcp") || !strings.Contains(string(cut), "FIXTURE-KEY-1") ||
		strings.Contains(string(cut), "atoz") {
		t.Fatalf("cut config = %s, %v", cut, err)
	}
	if fi, _ := os.Stat(o.MCPConfig); fi.Mode().Perm() != 0o600 {
		t.Errorf("cut config mode = %v", fi.Mode().Perm())
	}
	for _, tool := range []string{"mcp__slack-mcp__get_unread_counts", "mcp__slack-mcp__get_messages",
		"mcp__exa__web_search_exa", "mcp__exa__web_fetch_exa"} {
		if !slices.Contains(o.Tools, tool) || !slices.Contains(o.PermissionsAllow, tool) {
			t.Errorf("%s not offered and allowed: tools %q", tool, o.Tools)
		}
	}
	for _, tool := range []string{"mcp__slack-mcp__post_message", "mcp__slack-mcp__set_last_read"} {
		if slices.Contains(o.Tools, tool) || slices.Contains(o.PermissionsAllow, tool) || !slices.Contains(o.PermissionsDeny, tool) {
			t.Errorf("write tool %s not kept out", tool)
		}
	}
	for _, tool := range slices.Concat(o.Tools, o.PermissionsAllow) {
		if strings.Contains(tool, "atoz") || strings.Contains(tool, "aws-outlook-mcp") {
			t.Errorf("tool of a server not in the cut config: %s", tool)
		}
	}
	// The cut config holds the servers' secrets: no read rule may reach it.
	if !covered(o.PermissionsDeny, a.cfg.Home, o.MCPConfig) {
		t.Errorf("the cut config is readable: %q", o.PermissionsDeny)
	}

	// The live process is one a shim kept through a restart of naozhi:
	// what it started with is unknown (an older binary's scope), so it is
	// restarted on its conversation once, then left alone.
	ctx := context.Background()
	a.reconcile(ctx) // marks the conversation's shift
	a.reconcile(ctx)
	if sess.st.Alive {
		t.Fatal("a process of unknown scope was kept")
	}
	a.reconcile(ctx)
	a.reconcile(ctx)
	if !sess.st.Alive {
		t.Fatal("not resumed, or released again with the scope unchanged")
	}
	again := New(Config{Enabled: true, Layout: lay, Home: a.cfg.Home, Sessions: sess, MCPConfig: src,
		Now: func() time.Time { return now }})
	again.turns = &fakeTurns{}
	again.reconcile(ctx)
	if !sess.st.Alive {
		t.Fatal("after a restart of naozhi with the same scope: released")
	}
	if err := os.WriteFile(src, []byte(mcpDoc("FIXTURE-KEY-2")), 0o600); err != nil {
		t.Fatal(err)
	}
	a.reconcile(ctx)
	if cut, _ := os.ReadFile(o.MCPConfig); !strings.Contains(string(cut), "FIXTURE-KEY-2") {
		t.Fatalf("rotated key not written: %s", cut)
	}
	if sess.st.Alive {
		t.Fatal("idle Planner on the old key not released")
	}
	a.reconcile(ctx)
	a.reconcile(ctx)
	if !sess.st.Alive {
		t.Fatal("not resumed, or released again, on the new key")
	}
}

// Without a cli.mcp_config, or with one that defines none of its servers,
// the Planner starts on its built-in tools and no --mcp-config; a Planner
// that is off writes nothing.
func TestScopeNoMCP(t *testing.T) {
	now := time.Now()
	none := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(none, []byte(`{"mcpServers":{"atoz-mcp":{"command":"atoz"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"", none, filepath.Join(t.TempDir(), "missing.json")} {
		lay := datadir.FromRoot(t.TempDir())
		a := New(Config{Enabled: true, Layout: lay, Home: t.TempDir(), Sessions: &fakeSessions{}, MCPConfig: src,
			Now: func() time.Time { return now }})
		o := a.Opts()
		if o.MCPConfig != "" || !slices.Equal(o.Tools, Tools) {
			t.Errorf("mcp_config %q: opts.MCPConfig = %q, tools %q", src, o.MCPConfig, o.Tools)
		}
		// Not on naozhi's own settings file: an allow rule of the user's
		// settings would open with Bash, so the Planner has none.
		for _, rule := range o.PermissionsAllow {
			if strings.HasPrefix(rule, "Bash") {
				t.Errorf("Bash rule %s without the naozhi-owned settings", rule)
			}
		}
	}
	lay := datadir.FromRoot(t.TempDir())
	src := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(src, []byte(mcpDoc("FIXTURE-KEY-1")), 0o600); err != nil {
		t.Fatal(err)
	}
	New(Config{Layout: lay, Home: t.TempDir(), Sessions: &fakeSessions{}, MCPConfig: src})
	if _, err := os.Stat(lay.PlannerMCPConfigPath()); err == nil {
		t.Error("a Planner that is off wrote its MCP config")
	}
}
