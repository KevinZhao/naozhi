package main

import (
	"strings"
	"testing"
)

// The kind fixtures reuse the wire_egress throwaway module (stub clievent at
// its real module-relative path) and add the Kind* constants. KindBogus is not
// in the real kindTable this tool links, so it stands in for a constant whose
// table row was deleted.
const kindStub = `package clievent

const (
	KindUser     = "user"
	KindText     = "text"
	KindAgent    = "agent"
	KindTodo     = "todo"
	KindResult   = "result"
	KindToolUse  = "tool_use"
	KindBogus    = "bogus"
	NotAKind     = "text"
)
`

// histStub stands in for history.NewDerivedEntry, whose kind parameter the
// rule infers from its body.
const histStub = `package hist

import "example.com/fx/internal/cli/clievent"

func Derive(t string) clievent.EventEntry { return clievent.EventEntry{Type: t} }
`

const kindImports = `import (
	"example.com/fx/internal/cli/clievent"
	"example.com/fx/internal/hist"
)

var (
	_ clievent.EventEntry
	_ = hist.Derive
)
`

type kindCase struct {
	name string
	want string // violation | clean | nosite
	why  string // substring of the violation message, when it matters
	body string
}

var kindCases = []kindCase{
	// Every literal is rejected, a misspelling and a registered name alike.
	{"assigntypo", "violation", "string literal", `func F(e *clievent.EventEntry) { e.Type = "txt" }`},
	{"assignregistered", "violation", "string literal", `func F(e *clievent.EventEntry) { e.Type = "text" }`},
	{"literaltypo", "violation", "string literal", `func F() clievent.EventEntry { return clievent.EventEntry{Type: "txt"} }`},
	{"elided", "violation", "string literal", `var X = []clievent.EventEntry{{Type: "text"}}`},
	{"compare", "violation", "string literal", `func F(e clievent.EventEntry) bool { return e.Type == "tool_usee" }`},
	{"comparereversed", "violation", "string literal", `func F(e clievent.EventEntry) bool { return "text" != e.Type }`},
	{"switchcase", "violation", "string literal", `func F(e clievent.EventEntry) int {
	switch e.Type {
	case clievent.KindUser:
		return 1
	case "task_start":
		return 2
	}
	return 0
}`},
	{"embedded", "violation", "string literal", `type W struct{ clievent.EventEntry }

func F(w *W) { w.Type = "text" }`},
	// Kind parameters and kind variables carry the position to their sources.
	{"kindarg", "violation", "string literal", `func F() clievent.EventEntry { return hist.Derive("text") }`},
	{"kindvar", "violation", "string literal", `func F(user bool) clievent.EventEntry {
	var k string
	if user {
		k = "usr"
	} else {
		k = clievent.KindText
	}
	return hist.Derive(k)
}`},
	{"kindvarcompare", "violation", "string literal", `func F(user bool) bool {
	k := clievent.KindUser
	_ = hist.Derive(k)
	return k == "text"
}`},
	{"copyvar", "violation", "string literal", `func F(e clievent.EventEntry) bool {
	t := e.Type
	return t == "user"
}`},
	{"localparam", "violation", "string literal", `func derive(t string) clievent.EventEntry { return clievent.EventEntry{Type: t} }

func F() clievent.EventEntry { return derive("text") }`},
	// Constants other than registered clievent Kind*, and untraceable values.
	{"otherconst", "violation", "outside clievent's Kind*", `const k = "text"

func F(e *clievent.EventEntry) { e.Type = k }`},
	{"nonkindconst", "violation", "outside clievent's Kind*", `func F(e *clievent.EventEntry) { e.Type = clievent.NotAKind }`},
	{"unregistered", "violation", "not registered", `func F(e *clievent.EventEntry) { e.Type = clievent.KindBogus }`},
	{"field", "violation", "cannot trace", `type S struct{ k string }

func F(e *clievent.EventEntry, s S) { e.Type = s.k }`},
	{"global", "violation", "cannot trace", `var g = clievent.KindText

var X = clievent.EventEntry{Type: g}`},
	{"closureparam", "violation", "cannot trace", `func F() clievent.EventEntry {
	f := func(t string) clievent.EventEntry { return clievent.EventEntry{Type: t} }
	return f(clievent.KindText)
}`},
	{"call", "violation", "not a constant", `func kind() string { return clievent.KindText }

func F(e *clievent.EventEntry) { e.Type = kind() }`},

	// Constants, copies and traced parameters / variables, accepted.
	{"constants", "clean", "", `func F(e *clievent.EventEntry) int {
	e.Type = clievent.KindText
	_ = clievent.EventEntry{Type: clievent.KindUser}
	if e.Type == clievent.KindResult || clievent.KindToolUse != e.Type {
		return 1
	}
	switch e.Type {
	case clievent.KindAgent, clievent.KindTodo:
		return 2
	}
	return 0
}`},
	{"copy", "clean", "", `func F(a *clievent.EventEntry, b clievent.EventEntry) clievent.EventEntry {
	a.Type = b.Type
	return clievent.EventEntry{Type: b.Type}
}`},
	{"traced", "clean", "", `func derive(t string) clievent.EventEntry {
	if t == clievent.KindText {
		return clievent.EventEntry{}
	}
	return clievent.EventEntry{Type: t}
}

func F(e clievent.EventEntry, user bool) clievent.EventEntry {
	k := e.Type
	if user {
		k = clievent.KindUser
	}
	_ = derive(clievent.KindText)
	_ = hist.Derive(k)
	return derive(k)
}`},
	// Another type's Type field (cli.Event, ContentBlock) is not a kind.
	{"othertype", "nosite", "", `type Event struct{ Type string }

func F(e *Event) bool { e.Type = "result"; return e.Type == "text" }`},
}

func loadKindCases(t *testing.T) *typedProgram {
	t.Helper()
	files := map[string]string{"internal/cli/clievent/kinds.go": kindStub, "internal/hist/hist.go": histStub}
	for _, c := range kindCases {
		files["internal/cases/"+c.name+"/c.go"] = "package " + c.name + "\n\n" + kindImports + "\n" + c.body + "\n"
	}
	prog, err := loadTyped(writeFixtureModule(t, files))
	if err != nil {
		t.Fatal(err)
	}
	return prog
}

func TestEventEntryKind_Cases(t *testing.T) {
	t.Parallel()
	sites, vs := scanEventEntryKind(loadKindCases(t), nil)
	for _, v := range vs {
		if !strings.HasPrefix(v.File, "internal/cases/") {
			t.Errorf("unexpected rule-level violation: %+v", v)
		}
	}
	for _, c := range kindCases {
		dir := "internal/cases/" + c.name + "/"
		var nSites int
		var msgs []string
		for _, s := range sites {
			if strings.HasPrefix(s.File, dir) {
				nSites++
			}
		}
		for _, v := range vs {
			if strings.HasPrefix(v.File, dir) {
				msgs = append(msgs, v.Message)
			}
		}
		switch c.want {
		case "violation":
			if len(msgs) != 1 {
				t.Errorf("%s: want exactly one violation, got %d (%d sites): %q", c.name, len(msgs), nSites, msgs)
			} else if !strings.Contains(msgs[0], c.why) {
				t.Errorf("%s: violation %q does not say %q", c.name, msgs[0], c.why)
			}
		case "clean":
			if len(msgs) != 0 || nSites == 0 {
				t.Errorf("%s: want clean kind positions, got %d sites and violations %q", c.name, nSites, msgs)
			}
		case "nosite":
			if nSites != 0 {
				t.Errorf("%s: a non-EventEntry Type is not a kind position, got %d sites", c.name, nSites)
			}
		}
	}
}

// TestEventEntryKind_Sentinels: a sentinel file or package that holds no kind
// position fails; one that does stays quiet.
func TestEventEntryKind_Sentinels(t *testing.T) {
	t.Parallel()
	_, vs := scanEventEntryKind(loadKindCases(t), []string{"internal/cases/constants/c.go", "internal/cases/copy", "internal/cases/othertype"})
	var got []string
	for _, v := range vs {
		if strings.HasPrefix(v.Message, "sentinel ") {
			got = append(got, v.File)
		}
	}
	if len(got) != 1 || got[0] != "internal/cases/othertype" {
		t.Errorf("sentinel violations = %q, want only internal/cases/othertype", got)
	}
}
