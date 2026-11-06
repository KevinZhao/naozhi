package turn

import (
	"slices"
	"testing"
	"time"
)

// An Alone message is never merged: queued behind a running turn it runs
// by itself, and the messages queued around it merge without it, in order.
func TestDoneOrDrain_Alone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		queued  []Msg
		batches [][]string
	}{
		{"user after a scout wake", []Msg{{Text: "scout", Alone: true}, {Text: "user"}},
			[][]string{{"scout"}, {"user"}}},
		{"scout wake between", []Msg{{Text: "a"}, {Text: "b"}, {Text: "scout", Alone: true}, {Text: "c"}},
			[][]string{{"a", "b"}, {"scout"}, {"c"}}},
		{"two wakes", []Msg{{Text: "s1", Alone: true}, {Text: "s2", Alone: true}},
			[][]string{{"s1"}, {"s2"}}},
		{"none alone", []Msg{{Text: "a"}, {Text: "b"}}, [][]string{{"a", "b"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := newTestQueue(8, time.Millisecond)
			gen := q.Enqueue("k", Msg{Text: "running"}).gen
			for _, m := range tc.queued {
				q.Enqueue("k", m)
			}
			var got [][]string
			for b := q.DoneOrDrain("k", gen); b != nil; b = q.DoneOrDrain("k", gen) {
				var texts []string
				for _, m := range b {
					texts = append(texts, m.Text)
				}
				got = append(got, texts)
			}
			if !slices.EqualFunc(got, tc.batches, slices.Equal) {
				t.Errorf("batches = %q, want %q", got, tc.batches)
			}
		})
	}
}
