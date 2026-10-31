package main

import (
	"encoding/json"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

// TestBackgroundTickNeverBlocks 钉住：tick 处理必须是毫秒级、不做重活。
//
// 事故：tick 里同步跑云同步/设备探测，一旦某次调用不响应取消，宿主
// scheduler.loop 就永远到不了下一次 timer.Reset —— 实测表现为"注册成功、
// Immediate 首轮跑了、之后 30 秒周期的 lan-health 再也没触发"（整条调度停摆）。
// 因此 tick 只置位待处理标记，真正的活儿交给工具调用路径。
func TestBackgroundTickNeverBlocks(t *testing.T) {
	h := hosttest.New(t)
	udpCalls := 0
	h.On(plugin.OpUDPSend, func(raw json.RawMessage) (any, error) {
		udpCalls++
		return nil, errTestTimeout
	})
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}

	// lan-health tick：不得触发任何 UDP
	lanProbePending.Store(false)
	payload, _ := json.Marshal(map[string]any{"name": "lan-health"})
	if err := event(ctx, "smart.plugin.resource.background.task.tick", payload); err != nil {
		t.Fatalf("tick 失败: %v", err)
	}
	if udpCalls != 0 {
		t.Fatalf("tick 内不得做设备探测（发了 %d 次 UDP）", udpCalls)
	}
	if !lanProbePending.Load() {
		t.Fatal("tick 应置位待探测标记")
	}

	// account-sync tick：不得触发云同步
	cloudSyncPending.Store(false)
	payload2, _ := json.Marshal(map[string]any{"name": "account-sync"})
	if err := event(ctx, "smart.plugin.resource.background.task.tick", payload2); err != nil {
		t.Fatalf("tick 失败: %v", err)
	}
	if !cloudSyncPending.Load() {
		t.Fatal("tick 应置位待同步标记")
	}
}
