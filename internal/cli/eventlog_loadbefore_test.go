package cli

import (
	"context"
	"reflect"
	"testing"
)

// TestEventLogLoadBefore_MatchesEntriesBefore 钉死 LoadBefore 是
// EntriesBefore 的零逻辑薄封装（eventlog-subsystem-unify.md §8 Phase 1）：
// 任何输入下两者结果逐字节一致、error 恒为 nil。适配器若日后长出自己
// 的过滤/排序逻辑，此测试先红。
func TestEventLogLoadBefore_MatchesEntriesBefore(t *testing.T) {
	empty := NewEventLog(4)

	filled := NewEventLog(4)
	for i, ts := range []int64{1000, 2000, 3000} {
		filled.Append(EventEntry{Time: ts, Type: "user", Summary: string(rune('a' + i))})
	}

	// 溢出态：cap-4 ring 塞 6 条，最旧两条被挤出。
	overflow := NewEventLog(4)
	for i, ts := range []int64{100, 200, 300, 400, 500, 600} {
		overflow.Append(EventEntry{Time: ts, Type: "user", Summary: string(rune('a' + i))})
	}

	cases := []struct {
		name     string
		log      *EventLog
		beforeMS int64
		limit    int
	}{
		{"empty_no_bound", empty, 0, 10},
		{"single_page_all", filled, 0, 10},
		{"strictly_older", filled, 2000, 10},
		{"limit_truncates", filled, 0, 2},
		{"limit_zero", filled, 2000, 0},
		{"negative_before_no_bound", filled, -1, 10},
		{"overflow_ring", overflow, 0, 10},
		{"overflow_mid_page", overflow, 500, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.log.LoadBefore(context.Background(), tc.beforeMS, tc.limit)
			if err != nil {
				t.Fatalf("LoadBefore error = %v, want nil (ring never fails)", err)
			}
			want := tc.log.EntriesBefore(tc.beforeMS, tc.limit)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("LoadBefore = %+v, want EntriesBefore result %+v", got, want)
			}
		})
	}
}
