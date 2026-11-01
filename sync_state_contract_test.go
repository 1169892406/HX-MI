package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

// TestAccountViewReturnsLegacyFieldSet 钉住"配置页信息不再丢失"的契约：
// account 工具必须返回旧版 /v1/account 的那批字段。
//
// 事故背景：第一版只返回 state/relogin_required/device_count，前端于是没有
// 同步结果列表、三个计数、时间、失败次数可显示（用户："新版的丢失了很多信息"）。
// 本测试逐字段断言，缺一个就失败 —— 不靠"读代码相信"。
func TestAccountViewReturnsLegacyFieldSet(t *testing.T) {
	h := hosttest.New(t)
	state := syncStateRecord{
		Version: 1, LastOKAt: "2026-10-31T00:00:00Z", LastAttemptAt: "2026-10-31T00:00:00Z",
		ConsecutiveFailures: 2, LastError: "云接口超时",
		Devices: 2, Controllable: 1, Uncontrollable: 1,
		DeviceList: []syncDeviceView{
			{DID: "1", Name: "台灯", Model: "lumi.light", Controllable: true, Available: true, Online: true},
			{DID: "2", Name: "温度计", Model: "miaomiaoce.sensor", Reason: "no_address", ReasonText: "没有局域网地址"},
		},
	}
	stateB, _ := json.Marshal(state)
	sessionB, _ := json.Marshal(map[string]any{
		"version": 2, "region": "cn", "account": "1***@qq.com",
		"masked_user": "1***@qq.com", "saved_at": "2026-10-31T00:00:00Z", "tickets_sealed": true,
	})
	settingsB, _ := json.Marshal(hxSyncSettings{IntervalSeconds: 300, Enabled: true, HouseholdID: "hh"})
	devicesB, _ := json.Marshal(deviceRegistryV2{Version: 3, Devices: []miioDevice{
		{ExternalID: "1", Host: "192.168.1.10", TokenRef: "miio_token", Model: "lumi.light", Name: "台灯"},
		{ExternalID: "2", Host: "", TokenRef: "miio_token", Model: "miaomiaoce.sensor", Name: "温度计"},
	}})
	h.Fixture(plugin.OpHTTPOpen, map[string]any{"session_id": "hs-1"})
	h.Fixture(plugin.OpHTTPCookiesAttach, map[string]any{"attached": 3})
	h.Fixture(plugin.OpVaultBlobOpen, map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString([]byte(`{"ssecurity":"QUFB"}`))})
	h.Fixture(plugin.OpHTTPSessionRequest, plugin.HTTPSessionResponse{Status: 200, BodyB64: base64.StdEncoding.EncodeToString([]byte("QUFB"))})
	h.Fixture(plugin.OpHTTPClose, map[string]any{"ok": true})
	h.On(plugin.OpFSRead, func(raw json.RawMessage) (any, error) {
		var r struct{ Path string `json:"path"` }
		_ = json.Unmarshal(raw, &r)
		switch r.Path {
		case syncStatePath:
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(stateB)}, nil
		case "cloud-session.json":
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(sessionB)}, nil
		case hxSyncSettingsPath:
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(settingsB)}, nil
		case "devices.json":
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(devicesB)}, nil
		}
		return map[string]any{"found": false}, nil
	})

	out, err := call(&plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"},
		"account", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("account 调用失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("应答不是对象: %v", err)
	}

	// 身份三件：配置页账号行要用
	for _, k := range []string{"account", "region", "saved_at", "device_count", "state", "relogin_required"} {
		if _, ok := got[k]; !ok {
			t.Errorf("缺少顶层字段 %q（旧版有，配置页账号行依赖它）", k)
		}
	}

	sync, ok := got["sync"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 sync 块（同步结果列表/计数/时间全靠它）")
	}
	// 旧版 SyncStatusView 的 11 个字段
	want := []string{
		"enabled", "running", "devices", "controllable", "uncontrollable",
		"device_list", "last_ok_at", "next_sync_at", "consecutive_failures", "last_error",
	}
	for _, k := range want {
		if _, ok := sync[k]; !ok {
			t.Errorf("sync 缺少字段 %q", k)
		}
	}

	// 设备表：每行必须带名称/型号/可控/在线，且不可控要有原因文案
	list, _ := sync["device_list"].([]any)
	if len(list) != 2 {
		t.Fatalf("device_list 应有 2 台，实际 %d", len(list))
	}
	row := list[1].(map[string]any)
	if row["name"] != "温度计" || row["model"] != "miaomiaoce.sensor" {
		t.Errorf("设备行缺少名称/型号: %v", row)
	}
	if row["reason_text"] == "" || row["reason_text"] == nil {
		t.Errorf("不可控设备必须给原因文案: %v", row)
	}
	// 安全：不得出现令牌/地址
	for _, k := range []string{"token", "token_ref", "host", "ip", "ssecurity"} {
		if _, bad := row[k]; bad {
			t.Errorf("设备行泄露了敏感字段 %q", k)
		}
	}
}
