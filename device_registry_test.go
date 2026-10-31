package main

import (
	"encoding/base64"
	"encoding/json"
	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
	"testing"
)

func TestLoadRegistryV2(t *testing.T) {
	h := hosttest.New(t)
	b, _ := json.Marshal(deviceRegistryV2{Version: 2, Devices: []miioDevice{{ExternalID: "d", Host: "h"}}})
	h.Fixture(plugin.OpFSRead, map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(b)})
	d, e := loadDeviceRegistry(&plugin.Context{})
	if e != nil || len(d) != 1 || len(h.CallsOf(plugin.OpFSWrite)) != 0 {
		t.Fatal(d, e)
	}
}
func TestLoadRegistryLegacyNormalizes(t *testing.T) {
	h := hosttest.New(t)
	b, _ := json.Marshal([]miioDevice{{ExternalID: "d", Host: "h"}})
	h.Fixture(plugin.OpFSRead, map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(b)}).Fixture(plugin.OpFSWrite, map[string]any{"ok": true})
	d, e := loadDeviceRegistry(&plugin.Context{})
	if e != nil || len(d) != 1 || len(h.CallsOf(plugin.OpFSWrite)) != 1 {
		t.Fatal(d, e)
	}
}
func TestTerminalResourceError(t *testing.T) {
	for _, s := range []string{"PLUGIN_GENERATION_STALE", "PLUGIN_RESOURCE_EXHAUSTED", "PLUGIN_PERMISSION_DENIED"} {
		if !terminalResourceError(assertErr(s)) {
			t.Fatal(s)
		}
	}
	if terminalResourceError(assertErr("timeout")) {
		t.Fatal("timeout terminal")
	}
}
