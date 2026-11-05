package note

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/journal"
)

// A custody entry is a note on its session; one about custody as a whole
// (the pause) or with no event is none.
func TestFromEntryCustody(t *testing.T) {
	ts := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	id := fmt.Sprintf("c%d", ts.UnixNano())
	for _, c := range []struct {
		unit, data string
		want       *Note
	}{
		{"task:a", `{"event":"sent","reason":"出错重试","n":2}`, &Note{ID: id, Key: "task:a", Time: ts, Kind: KindCustody, Event: "sent", Evidence: "出错重试", N: 2}},
		{"task:a", `{"event":"deferred","reason":"你在看"}`, &Note{ID: id, Key: "task:a", Time: ts, Kind: KindCustody, Event: "deferred", Evidence: "你在看"}},
		{"task:a", `{"event":"on","by":"user"}`, &Note{ID: id, Key: "task:a", Time: ts, Kind: KindCustody, Event: "on", By: "user"}},
		{"", `{"event":"pause"}`, nil},
		{"task:a", `{}`, nil},
	} {
		got, ok := FromEntry(journal.Entry{TS: ts, Kind: journal.KindCustody, Unit: c.unit, Data: json.RawMessage(c.data)})
		if ok != (c.want != nil) || ok && got != *c.want {
			t.Errorf("FromEntry(%s %s) = %+v %v, want %+v", c.unit, c.data, got, ok, c.want)
		}
	}
}
