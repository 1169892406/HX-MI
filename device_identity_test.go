package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

func TestDeviceIDPersistedAndReused(t *testing.T) {
	h := hosttest.New(t)
	store := map[string][]byte{}
	h.On(plugin.OpFSRead, func(raw json.RawMessage) (any, error) {
		var r struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(raw, &r)
		if b, ok := store[r.Path]; ok {
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(b)}, nil
		}
		return map[string]any{"found": false}, nil
	})
	h.On(plugin.OpFSWrite, func(raw json.RawMessage) (any, error) {
		var w struct {
			Path    string `json:"path"`
			DataB64 string `json:"data_b64"`
		}
		_ = json.Unmarshal(raw, &w)
		b, _ := base64.StdEncoding.DecodeString(w.DataB64)
		store[w.Path] = b
		return map[string]any{"ok": true}, nil
	})
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	first, err := loadOrCreateDeviceID(ctx)
	if err != nil || !validDeviceID(first) {
		t.Fatalf("first=%q err=%v", first, err)
	}
	second, err := loadOrCreateDeviceID(ctx)
	if err != nil || second != first {
		t.Fatalf("复用失败 first=%q second=%q err=%v", first, second, err)
	}
	if !strings.Contains(string(store["device_id"]), first) {
		t.Fatalf("device_id 未落盘: %q", store["device_id"])
	}
}
