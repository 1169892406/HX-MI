package main

import (
	"testing"

	"homex.dev/homex/sdk/go/plugin"
)

// 老协议设备的能力裁剪（2026-11-24，用户「控制开关坏的，配置也是坏的」）。
//
// 钉住的事实：台灯 chuangmi.plug.m3 的云端 MIoT spec 声明了 service 3 指示灯，
// 但该固件既读不到（get_properties 稳定 -30001）也控不了它（set 3.1 会被当成主开关）。
// 参考实现 hass-xiaomi-miot 对这台设备只映射 2.1 power / 2.2 temperature，没有 3.1。
//
// 这个测试不碰 FS（loadLegacyDevices 在 FS 不可用时返回空表 ⇒ 非老协议 ⇒ 原样返回），
// 所以只钉"非老协议不动"这一侧；老协议那一侧由线上实测 + 下方白名单常量保证。
func TestFilterControlsForDevice_NonLegacyKeepsAll(t *testing.T) {
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	in := []map[string]any{
		{"siid": 2, "piid": 1, "capability": "switch", "name": "开关"},
		{"siid": 3, "piid": 1, "capability": "switch", "name": "指示灯 开关"},
	}
	out := filterControlsForDevice(ctx, "unknown-device", "chuangmi.plug.m3", in)
	if len(out) != 2 {
		t.Fatalf("非老协议设备不应裁剪能力：得到 %d 条，期望 2", len(out))
	}
}

// 白名单本身：必须且只能包含参考实现里这台设备真实支持的属性。
func TestLegacyWritableProps_Whitelist(t *testing.T) {
	if !legacyWritableProps["2.1"] || !legacyWritableProps["2.2"] {
		t.Fatal("老协议白名单必须包含 2.1 power 与 2.2 temperature")
	}
	if legacyWritableProps["3.1"] {
		t.Fatal("3.1 指示灯不得进白名单：该固件读不到也控不了（参考实现也没有它）")
	}
}

// 空 controls / 空 externalID 直接原样返回，不能 panic。
func TestFilterControlsForDevice_EmptyInputs(t *testing.T) {
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	if got := filterControlsForDevice(ctx, "x", "m", nil); got != nil {
		t.Fatalf("nil controls 应原样返回 nil，得到 %v", got)
	}
	in := []map[string]any{{"siid": 3, "piid": 1}}
	if got := filterControlsForDevice(ctx, "", "m", in); len(got) != 1 {
		t.Fatalf("空 externalID 应原样返回，得到 %d 条", len(got))
	}
}
