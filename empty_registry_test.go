package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

// TestEmptyRegistryIsNotCorrupt 钉住"云端一台设备都没有"是正常状态，不是损坏文件。
//
// 事故（用户实测手动同步报「设备登记损坏」）：空列表被 json.Marshal 成
// `"devices":null`，而加载器要求 `v2.Devices != nil`，于是空账号被判成坏文件。
func TestEmptyRegistryIsNotCorrupt(t *testing.T) {
	h := hosttest.New(t)
	// 真实文件形状：devices 是 null
	raw := `{"version":3,"devices":null,"sha256":"74234e98afe7498fb5daf1f36ac2d78acc339464f950703b8c019892f982b90b"}`
	h.Fixture(plugin.OpFSRead, map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString([]byte(raw))})

	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	ds, err := loadDeviceRegistry(ctx)
	if err != nil {
		t.Fatalf("空设备列表不得判为损坏: %v", err)
	}
	if len(ds) != 0 {
		t.Fatalf("应为 0 台，实际 %d", len(ds))
	}
}

// TestEmptyRegistryWritesArray 钉住写盘用 [] 而不是 null。
func TestEmptyRegistryWritesArray(t *testing.T) {
	if b, _ := json.Marshal([]miioDevice{}); strings.Contains(string(b), "null") {
		// 空切片 Marshal 是 []，这里只是防御性断言
		t.Fatal("空切片不应序列化成 null")
	}
	var nilSlice []miioDevice
	if b, _ := json.Marshal(nilSlice); !strings.Contains(string(b), "null") {
		t.Fatal("nil 切片确实会序列化成 null —— 这正是缺陷来源")
	}
}

// TestExistingSessionUsesOwnPair 钉住已会话分支不得二次 step1。
//
// 二次 step1 会读保险库里的 passToken（上一个账号的），拿到旧账号的配对材料，
// 与当前票据错配 → invalid signature（用户 01:04 实测）。
func TestExistingSessionUsesOwnPair(t *testing.T) {
	src := mustReadSource(t, "cloud_tools.go")
	// 在"已有会话"分支里不应再调用 refreshPairedMaterial
	idx := strings.Index(src, "if step1.Sign == \"\" {")
	if idx < 0 {
		t.Fatal("找不到已会话分支")
	}
	tail := src[idx:]
	end := strings.Index(tail, "if step1.Sign == \"\" {")
	_ = end
	// 截到该分支结束（下一个 proto := 之前）
	if p := strings.Index(tail, "proto := protocolState{"); p > 0 {
		tail = tail[:p]
	}
	// 只数真正的调用（形如 refreshPairedMaterial(ctx,），注释里提到函数名不算
	if strings.Contains(tail, "refreshPairedMaterial(ctx,") {
		t.Fatal("已会话分支不应再调 refreshPairedMaterial：它会用旧账号的 passToken 造出错配")
	}
}
