package reader

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

const goodReply = `{"answer":"M4 Max 的内存带宽是 546GB/s。","facts":[{"claim":"M4 Max 内存带宽 546GB/s","url":"https://www.apple.com/macbook-pro/specs/","quote":"546GB/s memory bandwidth"}],"confidence":"high"}`

func TestParseResult(t *testing.T) {
	for _, c := range []struct{ name, reply, err string }{
		{"ok", goodReply, ""},
		{"ok no facts", `{"answer":"查不到","facts":[],"confidence":"low"}`, ""},
		{"prose around", "结果如下：" + goodReply, "不是一个 JSON 对象"},
		{"fenced", "```json\n" + goodReply + "\n```", "不是一个 JSON 对象"},
		{"trailing", goodReply + " 好了", "后面还有内容"},
		{"unknown field", `{"answer":"a","facts":[],"confidence":"high","note":"x"}`, "不合格式"},
		{"empty answer", `{"answer":" ","facts":[],"confidence":"high"}`, "answer 是空的"},
		{"bad confidence", `{"answer":"a","facts":[],"confidence":"sure"}`, "confidence"},
		{"bad url", `{"answer":"a","facts":[{"claim":"c","url":"file:///etc/passwd","quote":"q"}],"confidence":"high"}`, "url"},
		{"no url", `{"answer":"a","facts":[{"claim":"c","url":"","quote":"q"}],"confidence":"high"}`, "url"},
		{"no claim", `{"answer":"a","facts":[{"claim":"","url":"https://e.com","quote":"q"}],"confidence":"high"}`, "claim"},
		// Truncated is the gateway's to set, not a field of the contract.
		{"truncated field", `{"answer":"a","facts":[],"confidence":"high","truncated":["answer"]}`, "不合格式"},
		{"over 64KB", `{"answer":"` + strings.Repeat("a", maxOutputBytes) + `","facts":[],"confidence":"high"}`, "超过 65536"},
		// A long answer does not excuse a bad URL.
		{"long answer bad url", `{"answer":"` + strings.Repeat("字", 1205) + `","facts":[{"claim":"c","url":"javascript:alert(1)","quote":"q"}],"confidence":"high"}`, "url"},
		{"not an object", `["answer"]`, "不是一个 JSON 对象"},
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

// TestParseResultTruncates: what is only too long is cut to the limits and
// named in Truncated, not failed (#3943: answers of 1205 and 707 runes
// failed whole runs under the old 600-rune limit).
func TestParseResultTruncates(t *testing.T) {
	fact := func(claim, url, quote string) string {
		return `{"claim":"` + claim + `","url":"` + url + `","quote":"` + quote + `"}`
	}
	t.Run("answer 707 fits", func(t *testing.T) {
		ans := strings.Repeat("跌", 707)
		res, err := ParseResult(`{"answer":"` + ans + `","facts":[],"confidence":"medium"}`)
		if err != nil || res.Answer != ans || res.Truncated != nil {
			t.Fatalf("res = %d runes, truncated %q, err %v; want it whole", utf8.RuneCountInString(res.Answer), res.Truncated, err)
		}
	})
	t.Run("answer 1205 cut", func(t *testing.T) {
		ans := strings.Repeat("新", 1205)
		res, err := ParseResult(`{"answer":"` + ans + `","facts":[` + fact("c", "https://e.com/a", "q") + `],"confidence":"high"}`)
		if err != nil {
			t.Fatal(err)
		}
		if n := utf8.RuneCountInString(res.Answer); n != AnswerRunes || !strings.HasSuffix(res.Answer, TruncatedMark) ||
			!strings.HasPrefix(ans, strings.TrimSuffix(res.Answer, TruncatedMark)) {
			t.Fatalf("answer = %d runes, ends %q; want %d with the mark", n, string([]rune(res.Answer)[n-8:]), AnswerRunes)
		}
		if strings.Join(res.Truncated, ",") != "answer" || len(res.Facts) != 1 {
			t.Fatalf("truncated = %q, facts %d", res.Truncated, len(res.Facts))
		}
	})
	t.Run("white space at the cut", func(t *testing.T) {
		keep := AnswerRunes - utf8.RuneCountInString(TruncatedMark)
		ans := strings.Repeat("a", keep-1) + "   " + strings.Repeat("b", 50)
		res, err := ParseResult(`{"answer":"` + ans + `","facts":[],"confidence":"high"}`)
		if err != nil || res.Answer != strings.Repeat("a", keep-1)+TruncatedMark {
			t.Fatalf("answer = %q, err %v", res.Answer, err)
		}
	})
	t.Run("facts past 8 dropped", func(t *testing.T) {
		var fs []string
		for i := range MaxFacts + 3 {
			fs = append(fs, fact("c"+strconv.Itoa(i+1), "https://e.com/"+strconv.Itoa(i+1), "q"))
		}
		// A dropped fact is not checked: its bad URL does not fail the run.
		fs[MaxFacts+1] = fact("bad", "file:///etc/passwd", "q")
		res, err := ParseResult(`{"answer":"a","facts":[` + strings.Join(fs, ",") + `],"confidence":"high"}`)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Facts) != MaxFacts || res.Facts[MaxFacts-1].Claim != "c8" || strings.Join(res.Truncated, ",") != "facts" {
			t.Fatalf("facts = %d, last %+v, truncated %q", len(res.Facts), res.Facts[len(res.Facts)-1], res.Truncated)
		}
	})
	t.Run("claim and quote cut", func(t *testing.T) {
		claim, quote := strings.Repeat("断", ClaimRunes+30), strings.Repeat("引", QuoteRunes+1)
		res, err := ParseResult(`{"answer":"a","facts":[` + fact("ok", "https://e.com/1", "ok") + "," + fact(claim, "https://e.com/2", quote) + `],"confidence":"low"}`)
		if err != nil {
			t.Fatal(err)
		}
		f := res.Facts[1]
		if f.Claim != strings.Repeat("断", ClaimRunes) || f.Quote != strings.Repeat("引", QuoteRunes) || res.Facts[0].Claim != "ok" {
			t.Fatalf("facts = %+v", res.Facts)
		}
		if strings.Join(res.Truncated, ",") != "facts[2].claim,facts[2].quote" {
			t.Fatalf("truncated = %q", res.Truncated)
		}
	})
}

// TestBlockTruncated: a cut answer reaches the Planner with a line saying
// what was cut, and its JSON line carries truncated; an uncut one has neither.
func TestBlockTruncated(t *testing.T) {
	res, err := ParseResult(`{"answer":"` + strings.Repeat("新", 1205) + `","facts":[],"confidence":"high"}`)
	if err != nil {
		t.Fatal(err)
	}
	b := Block(Run{ID: "20261110T170049-a85a", Q: "Fable 5.2 新特性", State: StateOK, Result: &res}, false)
	if !strings.Contains(b, "网关截断了 answer") || !strings.Contains(b, `"truncated":["answer"]`) || !strings.Contains(b, TruncatedMark) {
		t.Fatalf("block = %s", b)
	}
	ok, err := ParseResult(goodReply)
	if err != nil {
		t.Fatal(err)
	}
	if b := Block(Run{ID: "20261110T165033-37cb", Q: "q", State: StateOK, Result: &ok}, false); strings.Contains(b, "截断") || strings.Contains(b, "truncated") {
		t.Fatalf("uncut block = %s", b)
	}
}

// A long answer is cut, not failed: the run is ok, its record (what GET
// /api/planner/lookup serves) says what was cut, and so does the block.
func TestLookupLongAnswer(t *testing.T) {
	sess := &fakeSession{reply: `{"answer":"` + strings.Repeat("新", 1205) + `","facts":[{"claim":"c","url":"https://e.com","quote":"q"}],"confidence":"high"}`}
	shift := "s"
	now := time.Date(2026, 11, 10, 17, 0, 0, 0, time.Local)
	r, blocks := newReader(t, sess, &shift, &now)
	tk, err := r.Lookup("Fable 5.2 相比 5.1 的新特性", "")
	if err != nil {
		t.Fatal(err)
	}
	if b := wait(t, blocks); !strings.Contains(b, `state="ok"`) || !strings.Contains(b, "网关截断了 answer") {
		t.Fatalf("block = %s", b)
	}
	run, ok := r.Get(tk.Run)
	if !ok || run.State != StateOK || run.Result == nil || strings.Join(run.Result.Truncated, ",") != "answer" {
		t.Fatalf("record = %+v, %v", run, ok)
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
