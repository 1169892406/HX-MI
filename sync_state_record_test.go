package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

// TestRecordSyncSuccessPersistsSnapshot 钉住：同步成功后必须落盘设备清单与三计数。
// 不落盘的话配置页就只能显示空壳（用户："同步结果有列表的，新版丢失了"）。
func TestRecordSyncSuccessPersistsSnapshot(t *testing.T) {
	h := hosttest.New(t)
	var written []byte
	h.On(plugin.OpFSWrite, func(raw json.RawMessage) (any, error) {
		var w plugin.FSWriteRequest
		_ = json.Unmarshal(raw, &w)
		if w.Path == syncStatePath {
			written, _ = base64.StdEncoding.DecodeString(w.DataB64)
		}
		return map[string]any{"ok": true}, nil
	})
	h.Fixture(plugin.OpFSRead, map[string]any{"found": false})
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}

	devices := []syncDeviceView{
		{DID: "1", Name: "台灯", Model: "lumi.light", Controllable: true, Available: true, Online: true},
		{DID: "2", Name: "温度计", Model: "miaomiaoce.sensor", Available: true, Online: false,
			Reason: "no_address", ReasonText: "没有局域网地址"},
	}
	recordSyncSuccess(ctx, devices)

	if written == nil {
		t.Fatal("未写入 sync-state.json")
	}
	var st syncStateRecord
	if err := json.Unmarshal(written, &st); err != nil {
		t.Fatalf("落盘内容不是合法 JSON: %v", err)
	}
	if st.Devices != 2 || st.Controllable != 1 || st.Uncontrollable != 1 {
		t.Fatalf("三计数错误：devices=%d controllable=%d uncontrollable=%d",
			st.Devices, st.Controllable, st.Uncontrollable)
	}
	if len(st.DeviceList) != 2 {
		t.Fatalf("设备清单应 2 条，实际 %d", len(st.DeviceList))
	}
	if st.LastOKAt == "" {
		t.Fatal("未记录最近成功时间（配置页要显示它）")
	}
	if st.ConsecutiveFailures != 0 {
		t.Fatal("成功同步应清零连续失败")
	}
}

// TestRecordSyncFailureIncrementsAndKeepsError 钉住失败也要留痕：
// 连续失败次数与最后错误是"为什么一直没同步上"的唯一答案。
func TestRecordSyncFailureIncrementsAndKeepsError(t *testing.T) {
	h := hosttest.New(t)
	var written []byte
	h.On(plugin.OpFSWrite, func(raw json.RawMessage) (any, error) {
		var w plugin.FSWriteRequest
		_ = json.Unmarshal(raw, &w)
		if w.Path == syncStatePath {
			written, _ = base64.StdEncoding.DecodeString(w.DataB64)
		}
		return map[string]any{"ok": true}, nil
	})
	prev, _ := json.Marshal(syncStateRecord{Version: 1, ConsecutiveFailures: 2, Devices: 5, Controllable: 3})
	h.On(plugin.OpFSRead, func(raw json.RawMessage) (any, error) {
		var r struct{ Path string `json:"path"` }
		_ = json.Unmarshal(raw, &r)
		if r.Path == syncStatePath {
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(prev)}, nil
		}
		return map[string]any{"found": false}, nil
	})
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}

	recordSyncFailure(ctx, errString("云接口超时"))

	var st syncStateRecord
	if err := json.Unmarshal(written, &st); err != nil {
		t.Fatalf("落盘内容不是合法 JSON: %v", err)
	}
	if st.ConsecutiveFailures != 3 {
		t.Fatalf("连续失败应递增到 3，实际 %d", st.ConsecutiveFailures)
	}
	if st.LastError != "云接口超时" {
		t.Fatalf("最后错误未保留：%q", st.LastError)
	}
	// 失败不能抹掉上一次成功的设备清单（否则界面会突然变空，误导用户）
	if st.Devices != 5 {
		t.Fatalf("失败不应清空上次的设备计数，实际 %d", st.Devices)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
