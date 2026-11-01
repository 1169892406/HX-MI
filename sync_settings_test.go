package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

func TestSyncSettingsDefaultThenPersist(t *testing.T) {
	h := hosttest.New(t)
	var wrote []byte
	h.Fixture(plugin.OpFSRead, map[string]any{"found": false})
	h.On(plugin.OpFSWrite, func(raw json.RawMessage) (any, error) {
		var w struct {
			Path    string `json:"path"`
			DataB64 string `json:"data_b64"`
		}
		_ = json.Unmarshal(raw, &w)
		wrote, _ = base64.StdEncoding.DecodeString(w.DataB64)
		return map[string]any{"ok": true}, nil
	})
	h.Fixture(plugin.OpBackgroundStart, map[string]any{"ok": true})
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh-settings"}

	out, err := applySyncSettingsTool(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	_ = json.Unmarshal(out, &v)
	if v["interval_seconds"].(float64) != 300 || v["household_editable"] != false || v["household_id"] != "hh-settings" {
		t.Fatalf("default view=%v", v)
	}

	out, err = applySyncSettingsTool(ctx, json.RawMessage(`{"interval_seconds":120,"enabled":false}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(out, &v)
	if v["interval_seconds"].(float64) != 120 || v["enabled"] != false {
		t.Fatalf("updated view=%v", v)
	}
	if len(wrote) == 0 || !strings.Contains(string(wrote), `"interval_seconds":120`) {
		t.Fatalf("persisted=%s", wrote)
	}
	if len(h.CallsOf(plugin.OpBackgroundStart)) != 1 {
		t.Fatalf("background re-register=%d", len(h.CallsOf(plugin.OpBackgroundStart)))
	}
}

func TestSyncSettingsRejectsOutOfRange(t *testing.T) {
	h := hosttest.New(t)
	h.Fixture(plugin.OpFSRead, map[string]any{"found": false})
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	if _, err := applySyncSettingsTool(ctx, json.RawMessage(`{"interval_seconds":30}`)); err == nil {
		t.Fatal("30 秒应被拒绝")
	}
	if len(h.CallsOf(plugin.OpFSWrite)) != 0 {
		t.Fatal("非法值不应写盘")
	}
}
