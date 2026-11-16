package wireup

import (
	"slices"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/planner/digest"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/session"
)

// TestTaint: a turn that searched the web or read a mail marks the session
// tainted in its sidecar (sticky), which the Planner's deny list reads
// (Store.Tainted) and its digest heads with 含外部内容; a clean session is
// left alone.
func TestTaint(t *testing.T) {
	r := newDigestRouter(t)
	sc := sidecar.Open(t.TempDir(), nil)
	const web, clean, internal = "dashboard:direct:u:general", "dashboard:direct:u:other", "dashboard:direct:u:wiki"
	wp := session.NewTestProcess()
	wp.EventLog.Append(clievent.EventEntry{Time: 1, Type: clievent.KindUser, Detail: "查一下"})
	wp.EventLog.Append(clievent.EventEntry{Time: 2, Type: clievent.KindToolUse, Tool: "mcp__exa__web_search_exa"})
	wp.EventLog.Append(clievent.EventEntry{Time: 3, Type: clievent.KindToolUse, Tool: "mcp__aws-outlook-mcp__email_read"})
	r.InjectSession(web, wp)
	cp := session.NewTestProcess()
	cp.EventLog.Append(clievent.EventEntry{Time: 1, Type: clievent.KindToolUse, Tool: "Read"})
	r.InjectSession(clean, cp)
	// An internal page read through builder-mcp is outside content too; its
	// other tools (code search, a ticket's read) are not page fetches.
	ip := session.NewTestProcess()
	ip.EventLog.Append(clievent.EventEntry{Time: 1, Type: clievent.KindToolUse, Tool: "mcp__builder-mcp__InternalCodeSearch"})
	ip.EventLog.Append(clievent.EventEntry{Time: 2, Type: clievent.KindToolUse, Tool: "mcp__builder-mcp__ReadInternalWebsites"})
	r.InjectSession(internal, ip)

	taint := &Taint{Router: r, Sidecars: sc}
	taint.Check(web)
	taint.Check(clean)
	taint.Check(internal)
	got := sc.Tainted()
	slices.Sort(got)
	if !slices.Equal(got, []string{web, internal}) {
		t.Fatalf("tainted = %q, want [%s %s]", got, web, internal)
	}
	if m, _ := sc.Get(internal); !slices.Equal(m.Taint.Kinds, []string{sidecar.TaintWeb}) || !slices.Equal(m.Taint.Tools, []string{"mcp__builder-mcp__ReadInternalWebsites"}) {
		t.Errorf("internal page read: taint = %+v", m.Taint)
	}
	m, _ := sc.Get(web)
	if !slices.Equal(m.Taint.Kinds, []string{sidecar.TaintWeb, sidecar.TaintEmail}) {
		t.Errorf("kinds = %q", m.Taint.Kinds)
	}
	f, ok := DigestSources{Router: r, Sidecars: sc}.Facts(web)
	if !ok {
		t.Fatal("no digest facts")
	}
	md := string(digest.Render(f))
	if !strings.Contains(md, "taint: [\"web\",\"email\"]") || !strings.Contains(md, digest.TaintNote) {
		t.Errorf("digest does not carry the mark:\n%s", md)
	}
}
