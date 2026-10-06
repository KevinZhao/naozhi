package costledger

import (
	"testing"
	"time"
)

func TestInChat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		session, chat string
		want          bool
	}{
		{"feishu:direct:a:general", "feishu:direct:a", true},
		{"feishu:direct:a:code-reviewer", "feishu:direct:a", true},
		{"feishu:direct:ab:general", "feishu:direct:a", false}, // prefix of another chat
		{"feishu:direct:a", "feishu:direct:a", false},          // no agent tail
		{"feishu:direct:a:x:y", "feishu:direct:a", false},      // not an IM key shape
		{"project:p:planner", "feishu:direct:a", false},
	}
	for _, c := range cases {
		if got := inChat(c.session, c.chat); got != c.want {
			t.Errorf("inChat(%q, %q) = %v, want %v", c.session, c.chat, got, c.want)
		}
	}
}

func TestSummarize_ChatKeyFilterSpansAgents(t *testing.T) {
	s, _ := newTestStore(t, t0)
	ents := []Entry{
		mk(t0.Add(-2*time.Hour), SourceSession, UnitUSD, 1.0),
		mk(t0.Add(-1*time.Hour), SourceSession, UnitUSD, 0.5),
		mk(t0.Add(-1*time.Hour), SourceSession, UnitUSD, 4.0),
		mk(t0.Add(-1*time.Hour), SourceSession, UnitCredits, 7.0),
	}
	ents[0].SessionKey = "feishu:group:oc_1:general"
	ents[1].SessionKey = "feishu:group:oc_1:code-reviewer"
	ents[2].SessionKey = "feishu:group:oc_12:general" // same prefix, other chat
	ents[3].SessionKey = "feishu:group:oc_1:general"  // other unit, same chat
	for i, e := range ents {
		if !s.Append(e) {
			t.Fatalf("append %d rejected", i)
		}
	}
	s.Close()

	sum, err := s.Summarize(Query{
		From: t0.Add(-24 * time.Hour), To: t0.Add(time.Minute),
		GroupBy: GroupByUnit, ChatKey: "feishu:group:oc_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := bucket(sum, string(UnitUSD), UnitUSD); !ok || b.Amount != 1.5 || b.Entries != 2 {
		t.Fatalf("USD bucket = %+v ok=%v, want 1.5 over 2 entries", b, ok)
	}
	if b, ok := bucket(sum, string(UnitCredits), UnitCredits); !ok || b.Amount != 7.0 {
		t.Fatalf("credits bucket = %+v ok=%v", b, ok)
	}
}
