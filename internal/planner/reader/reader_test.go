package reader

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

const goodReply = `{"answer":"M4 Max 的内存带宽是 546GB/s。","facts":[{"claim":"M4 Max 内存带宽 546GB/s","url":"https://www.apple.com/macbook-pro/specs/","quote":"546GB/s memory bandwidth"}],"confidence":"high"}`

func TestParseResult(t *testing.T) {
	long := strings.Repeat("字", AnswerRunes+1)
	for _, c := range []struct{ name, reply, err string }{
		{"ok", goodReply, ""},
		{"ok no facts", `{"answer":"查不到","facts":[],"confidence":"low"}`, ""},
		{"prose around", "结果如下：" + goodReply, "不是一个 JSON 对象"},
		{"fenced", "```json\n" + goodReply + "\n```", "不是一个 JSON 对象"},
		{"trailing", goodReply + " 好了", "后面还有内容"},
		{"unknown field", `{"answer":"a","facts":[],"confidence":"high","note":"x"}`, "不合格式"},
		{"empty answer", `{"answer":" ","facts":[],"confidence":"high"}`, "answer 是空的"},
		{"long answer", `{"answer":"` + long + `","facts":[],"confidence":"high"}`, "超过 600"},
		{"bad confidence", `{"answer":"a","facts":[],"confidence":"sure"}`, "confidence"},
		{"bad url", `{"answer":"a","facts":[{"claim":"c","url":"file:///etc/passwd","quote":"q"}],"confidence":"high"}`, "url"},
		{"long quote", `{"answer":"a","facts":[{"claim":"c","url":"https://e.com","quote":"` + strings.Repeat("q", QuoteRunes+1) + `"}],"confidence":"high"}`, "quote"},
		{"no claim", `{"answer":"a","facts":[{"claim":"","url":"https://e.com","quote":"q"}],"confidence":"high"}`, "claim"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseResult(c.reply)
			switch {
			case c.err == "" && err != nil:
				t.Fatalf("err = %v", err)
			case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
				t.Fatalf("err = %v, want one with %q", err, c.err)
			}
		})
	}
}

// fakeSession answers every reader session with reply, counting the runs.
type fakeSession struct {
	mu    sync.Mutex
	reply string
	keys  []string
	opts  []sessionview.AgentOpts
}

func (f *fakeSession) run(_ context.Context, key string, opts sessionview.AgentOpts, _ string) (string, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys, f.opts = append(f.keys, key), append(f.opts, opts)
	return f.reply, []string{"mcp__exa__web_search_exa"}, nil
}

func newReader(t *testing.T, sess *fakeSession, shift *string, now *time.Time) (*Reader, chan string) {
	t.Helper()
	root := t.TempDir()
	mcp := filepath.Join(root, "mcp.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"exa":{"type":"stdio","command":"x"},"aws-outlook-mcp":{"type":"stdio","command":"y"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	blocks := make(chan string, 64)
	r := New(Config{
		Layout:    datadir.FromRoot(root),
		MCPConfig: mcp,
		Session:   sess.run,
		Deliver:   func(b string) error { blocks <- b; return nil },
		Shift:     func() string { return *shift },
		Now:       func() time.Time { return *now },
	})
	t.Cleanup(r.Stop)
	return r, blocks
}

func wait(t *testing.T, blocks chan string) string {
	t.Helper()
	select {
	case b := <-blocks:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("no block delivered")
		return ""
	}
}

// TestLookupCacheAndLimit: an answer reaches the Planner in a
// <naozhi-lookup> block; the same question asked again the same day comes
// from the cache with no session, the next day runs again; a shift runs at
// most MaxPerShift lookups, and the next shift starts afresh.
func TestLookupCacheAndLimit(t *testing.T) {
	sess := &fakeSession{reply: goodReply}
	shift := "2026-11-10@a"
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local)
	r, blocks := newReader(t, sess, &shift, &now)

	tk, err := r.Lookup("M4 Max 内存带宽", "用户问")
	if err != nil || tk.Cached {
		t.Fatalf("Lookup = %+v, %v", tk, err)
	}
	b := wait(t, blocks)
	if !strings.HasPrefix(b, `<naozhi-lookup q="M4 Max 内存带宽" run="`+tk.Run+`" state="ok">`) || !strings.Contains(b, "546GB/s") {
		t.Fatalf("block = %s", b)
	}
	if run, ok := r.Get(tk.Run); !ok || run.State != StateOK || run.Result == nil {
		t.Fatalf("record = %+v, %v", run, ok)
	}
	// The reader gets the web search servers only, no other MCP server.
	if o := sess.opts[0]; !o.StrictMCPConfig || o.PermissionMode != "dontAsk" || strings.Join(o.Tools, ",") != "mcp__exa__web_search_exa,mcp__exa__web_fetch_exa" {
		t.Errorf("opts = %+v", o)
	}
	if data, _ := os.ReadFile(r.cfg.Layout.ReaderMCPConfigPath()); strings.Contains(string(data), "outlook") {
		t.Errorf("reader MCP config carries another server: %s", data)
	}

	tk2, err := r.Lookup("  m4 max   内存带宽 ", "")
	if err != nil || !tk2.Cached || tk2.Run != tk.Run {
		t.Fatalf("same question again = %+v, %v; want the cache's", tk2, err)
	}
	if b := wait(t, blocks); !strings.Contains(b, `cached="1"`) {
		t.Fatalf("cached block = %s", b)
	}
	if len(sess.keys) != 1 {
		t.Fatalf("sessions = %q, want one", sess.keys)
	}

	now = now.Add(24 * time.Hour)
	if tk3, err := r.Lookup("M4 Max 内存带宽", ""); err != nil || tk3.Cached {
		t.Fatalf("next day = %+v, %v; want a fresh run", tk3, err)
	}
	wait(t, blocks)

	for i := 2; i < MaxPerShift; i++ {
		if _, err := r.Lookup("问题 "+string(rune('A'+i)), ""); err != nil {
			t.Fatalf("lookup %d: %v", i+1, err)
		}
		wait(t, blocks)
	}
	if _, err := r.Lookup("再一个", ""); err != ErrLimit {
		t.Fatalf("lookup past the limit: err = %v, want ErrLimit", err)
	}
	shift = "2026-11-11@b"
	if _, err := r.Lookup("再一个", ""); err != nil {
		t.Fatalf("next shift: %v", err)
	}
	wait(t, blocks)
}

// A reply that breaks the contract fails the run, and the Planner is told.
func TestLookupBadOutput(t *testing.T) {
	sess := &fakeSession{reply: "我查到了：546GB/s"}
	shift := "s"
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local)
	r, blocks := newReader(t, sess, &shift, &now)
	tk, err := r.Lookup("q", "")
	if err != nil {
		t.Fatal(err)
	}
	if b := wait(t, blocks); !strings.Contains(b, `state="failed"`) || !strings.Contains(b, "不合格") {
		t.Fatalf("block = %s", b)
	}
	if run, _ := r.Get(tk.Run); run.State != StateFailed {
		t.Fatalf("record state = %q", run.State)
	}
	// A failed answer is not cached.
	if tk2, _ := r.Lookup("q", ""); tk2.Cached {
		t.Fatal("failed answer served from the cache")
	}
	wait(t, blocks)
}
