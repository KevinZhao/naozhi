// hub_concurrency_test.go — Phase 4a 骨架的最小测试。
//
// Phase 4a 范围（server-split-phase4-design v0.6.1 §6.5）：1 个测试 case
// 验证 NewHub + Shutdown 协调链路设计正确。Phase 4b PR 必须新增完整
// 并发测试（broadcast 中触发 Shutdown / Register 与 Shutdown 并发 /
// send 与 broadcast 同时进行）+ 跑 -race -count=100。
//
// 4a 骨架阶段：本测试仅证明 Hub struct 字段初始化无误 + Shutdown 5 步
// 协调链路的字段写入顺序无 panic / 无 deadlock。
package wshub

import (
	"context"
	"testing"
	"time"
)

// TestNewHubShutdown_Skeleton verifies the Phase 4a skeleton's lifecycle
// coordination (NewHub → Shutdown 5-step ordering) does not deadlock.
//
// Phase 4b 升级目标（hub_concurrency_test.go full version）：
//   - broadcast 中触发 Shutdown
//   - Register 与 Shutdown 并发
//   - send 与 broadcast 同时进行
//   - 跑 -race -count=100
func TestNewHubShutdown_Skeleton(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h := NewHub(HubOptions{ParentCtx: ctx})
	if h == nil {
		t.Fatal("NewHub returned nil")
	}

	// 验证 lifecycle 字段已初始化
	if h.ctx == nil {
		t.Error("h.ctx not initialized after NewHub")
	}
	if h.cancel == nil {
		t.Error("h.cancel not initialized after NewHub")
	}

	// 验证 subscriber 块字段已初始化
	if h.clients == nil {
		t.Error("h.clients map not initialized")
	}
	if h.subscriberCount == nil {
		t.Error("h.subscriberCount map not initialized")
	}

	// 验证 rate-limit/cache 块字段已初始化
	if h.userSendLimiters == nil {
		t.Error("h.userSendLimiters map not initialized")
	}
	if h.connCountByOwner == nil {
		t.Error("h.connCountByOwner map not initialized")
	}

	// 验证 agent tailer 块字段已初始化
	if h.wiredLinkers == nil {
		t.Error("h.wiredLinkers map not initialized")
	}

	// 验证 broadcast 块的 debounceFire pre-binding
	if h.debounceFire == nil {
		t.Error("h.debounceFire not pre-bound by NewHub (expected per R239-PERF-6)")
	}

	// 调用 Shutdown，验证 5 步协调链路无死锁。
	done := make(chan struct{})
	go func() {
		_ = h.Shutdown(context.Background())
		close(done)
	}()

	select {
	case <-done:
		// 成功退出
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return within 2s — possible deadlock in skeleton lifecycle coordination")
	}

	// 验证 Shutdown 后状态
	if !h.debounceClosed {
		t.Error("debounceClosed not set after Shutdown")
	}
	if !h.debounceClosedFast.Load() {
		t.Error("debounceClosedFast not set after Shutdown")
	}
	if !h.sendClosed {
		t.Error("sendClosed not set after Shutdown")
	}
	if h.clients != nil {
		t.Error("h.clients should be nil'd by Shutdown (subscriber block teardown)")
	}
}

// TestShutdownIdempotent verifies Shutdown can be called multiple times
// without panic. Lifecycle methods MUST be idempotent (R183-CONCUR-M1
// 同款约束在 cli/Process.OnTurnDone)。
func TestShutdownIdempotent(t *testing.T) {
	t.Parallel()

	h := NewHub(HubOptions{ParentCtx: context.Background()})

	if err := h.Shutdown(context.Background()); err != nil {
		t.Errorf("first Shutdown returned error: %v", err)
	}
	// 第二次调用——必须不 panic、不死锁
	if err := h.Shutdown(context.Background()); err != nil {
		t.Errorf("second Shutdown returned error: %v", err)
	}
}

// TestNewHubNilParentCtx verifies NewHub falls back to context.Background()
// when ParentCtx is nil (legacy behaviour for tests / headless wiring).
func TestNewHubNilParentCtx(t *testing.T) {
	t.Parallel()

	h := NewHub(HubOptions{}) // no ParentCtx
	if h == nil {
		t.Fatal("NewHub returned nil with empty options")
	}
	if h.ctx == nil {
		t.Error("h.ctx not initialized when ParentCtx is nil")
	}

	if err := h.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown returned error: %v", err)
	}
}

// TestParentCtxCancel verifies that cancelling ParentCtx propagates to
// h.ctx (the child ctx derived in NewHub). This is the CTX1 contract:
// a parent-ctx cancel must tear down Hub goroutines even if Shutdown()
// is never explicitly called.
func TestParentCtxCancel(t *testing.T) {
	t.Parallel()

	parent, cancelParent := context.WithCancel(context.Background())
	h := NewHub(HubOptions{ParentCtx: parent})

	// h.ctx 应该跟随 parent
	if h.ctx.Err() != nil {
		t.Fatal("h.ctx should not be cancelled before parent")
	}
	cancelParent()

	// h.ctx 应该立即被取消
	select {
	case <-h.ctx.Done():
		// 正确：parent cancel 传播到 child
	case <-time.After(1 * time.Second):
		t.Fatal("h.ctx not cancelled within 1s of parent cancel")
	}
}
