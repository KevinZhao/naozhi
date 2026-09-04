// 编译期接口断言 + round-trip 契约测试——eventlog-subsystem-unify.md
// §4.1 接口轴 / §8 Phase 1。把"四个 tier 概念同形"从文档声明升级为
// CI 闸门：任何后端签名漂移在此包 go build/test 即失败，防 #1369
// 影子层再生。
//
// 放在 api 的外部测试包：api 生产代码只 import cli（见 api.go 注），
// 断言需要的 naozhilog/merged 边只存在于测试，且两包均不反向 import
// api，无环（§4.1 的放置预案 a）。
package api_test

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/api"
	"github.com/naozhi/naozhi/internal/history/merged"
	"github.com/naozhi/naozhi/internal/history/naozhilog"
)

// ring：三分面全满足 → EventStore。LoadBefore 缺口由
// cli/eventlog_loadbefore.go 的 thin adapter 补齐（Phase 1 预案）。
var (
	_ api.Appender   = (*cli.EventLog)(nil)
	_ api.Subscriber = (*cli.EventLog)(nil)
	_ api.Reader     = (*cli.EventLog)(nil)
	_ api.EventStore = (*cli.EventLog)(nil)
)

// durable / replay tier：只读分面。写侧走 persist.Entry（bridge 转换），
// 不参与 Appender 断言——见 api.go 对 bridge 剩余职责的定位。
var (
	_ api.Reader = (*naozhilog.Source)(nil)
	_ api.Reader = (*merged.Source)(nil)
)

// TestRingRoundTripViaEventStore 通过 api.EventStore 接口（而非具体
// *cli.EventLog）验证 §4.1 的顺序契约：Append → SubscribeNew 收到通知 →
// Reader.LoadBefore 读回 oldest→newest。覆盖空 / 单条 / 批量 / ring
// 溢出四态。
func TestRingRoundTripViaEventStore(t *testing.T) {
	var store api.EventStore = cli.NewEventLog(4)

	sub := store.SubscribeNew()
	defer sub.Cancel()

	// 空态：无上界读返回空。
	if got, err := store.LoadBefore(context.Background(), 0, 10); err != nil || len(got) != 0 {
		t.Fatalf("empty store: LoadBefore = %v, %v; want empty, nil", got, err)
	}

	// 单条：Append 后通知到达、可读回。
	store.Append(clievent.EventEntry{Time: 1000, Type: "user", Summary: "a"})
	select {
	case <-sub.Notify():
	default:
		t.Fatal("SubscribeNew channel did not fire on Append (contract: fires non-blocking on every Append)")
	}
	got, err := store.LoadBefore(context.Background(), 0, 10)
	if err != nil || len(got) != 1 || got[0].Summary != "a" {
		t.Fatalf("after single Append: LoadBefore = %+v, %v; want [a], nil", got, err)
	}

	// 批量：AppendBatch 保序追加。
	store.AppendBatch([]clievent.EventEntry{
		{Time: 2000, Type: "user", Summary: "b"},
		{Time: 3000, Type: "user", Summary: "c"},
	})
	got, err = store.LoadBefore(context.Background(), 0, 10)
	if err != nil || len(got) != 3 {
		t.Fatalf("after batch: LoadBefore = %+v, %v; want 3 entries, nil", got, err)
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i].Summary != want {
			t.Errorf("order[%d] = %q, want %q (contract: oldest → newest)", i, got[i].Summary, want)
		}
	}

	// 溢出：cap-4 ring 第 5 条挤出最旧的 "a"。
	store.Append(clievent.EventEntry{Time: 4000, Type: "user", Summary: "d"})
	store.Append(clievent.EventEntry{Time: 5000, Type: "user", Summary: "e"})
	got, err = store.LoadBefore(context.Background(), 0, 10)
	if err != nil || len(got) != 4 {
		t.Fatalf("after overflow: LoadBefore = %+v, %v; want 4 entries, nil", got, err)
	}
	for i, want := range []string{"b", "c", "d", "e"} {
		if got[i].Summary != want {
			t.Errorf("overflow order[%d] = %q, want %q", i, got[i].Summary, want)
		}
	}
}
