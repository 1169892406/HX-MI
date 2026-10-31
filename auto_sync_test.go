package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

// TestAccountSyncTickDoesNotSpawnHostCallGoroutine 钉住：tick 里绝不能起
// 会调用 host_call 的 goroutine。
//
// 事故（实测）：tick 里 `go runAsyncSync(ctx)` 后，那个 goroutine 立刻要
// wasm 实例锁（CallOp 串行），而 tick 尚未返回、锁仍被持有 ⇒ 永久死锁。
// 现象：03:59 之后再无同步成功，每轮 tick 打印 "previous sync still running"，
// sync-state.json 永远不生成。
func TestAccountSyncTickDoesNotSpawnHostCallGoroutine(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	idx := strings.Index(s, `if tick.Name == "account-sync" {`)
	if idx < 0 {
		t.Fatal("找不到 account-sync 分支")
	}
	tail := s[idx:]
	if end := strings.Index(tail, `ctx.Log(0, "hx-mi background tick: "`); end > 0 {
		tail = tail[:end]
	}
	// 只检查**代码行**，注释里提到这个写法（解释为什么不能用）不算违规
	for _, line := range strings.Split(tail, "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "//") {
			continue
		}
		if strings.Contains(code, "go runAsyncSync") || strings.HasPrefix(code, "go func()") {
			t.Fatalf("tick 分支不得起 goroutine 做 host_call：会与串行实例锁自死锁：%s", code)
		}
	}
}

// TestTickSetsPendingWithoutTouchingHost 钉住 tick 只置位、不碰 host。
func TestTickSetsPendingWithoutTouchingHost(t *testing.T) {
	h := hosttest.New(t)
	hostCalls := 0
	h.On(plugin.OpFSRead, func(raw json.RawMessage) (any, error) {
		hostCalls++
		return map[string]any{"found": false}, nil
	})
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	cloudSyncPending.Store(false)
	payload, _ := json.Marshal(map[string]any{"name": "account-sync"})
	if err := event(ctx, "smart.plugin.resource.background.task.tick", payload); err != nil {
		t.Fatal(err)
	}
	if hostCalls != 0 {
		t.Fatalf("tick 里不得发起 host_call（实际 %d 次）——会死锁", hostCalls)
	}
	if !cloudSyncPending.Load() {
		t.Fatal("tick 应置位待同步标记")
	}
}

// TestReadOnlyToolConsumesPendingSync 钉住标记会被只读工具调用消费。
func TestReadOnlyToolConsumesPendingSync(t *testing.T) {
	src, _ := os.ReadFile("main.go")
	s := string(src)
	if !strings.Contains(s, "consumePendingAutoSync(ctx)") {
		t.Fatal("没有消费点：标记置了没人执行，等于自动同步不存在")
	}
	// 改凭据的工具不应触发
	for _, tool := range []string{`tool != "login"`, `tool != "verify"`, `tool != "logout"`} {
		if !strings.Contains(s, tool) {
			t.Fatalf("应排除改凭据的工具以免干扰：%s", tool)
		}
	}
}
