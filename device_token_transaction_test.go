package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
	"testing"
)

func TestLegacyTransactionDevicesBeforeCommit(t *testing.T) {
	h := hosttest.New(t)
	legacy := legacyDevices()
	sum := sha256.Sum256(legacy)
	h.Fixture(plugin.OpLegacyStatus, plugin.LegacyStatusResponse{}).Fixture(plugin.OpLegacySnapshot, plugin.LegacySnapshotResponse{SnapshotID: "s"}).Fixture(plugin.OpLegacyList, map[string]any{"files": []plugin.LegacyFileInfo{{Path: "devices.json", Size: int64(len(legacy)), SHA256: base64.StdEncoding.EncodeToString(sum[:])}}}).Fixture(plugin.OpLegacyRead, map[string]any{"data_b64": base64.StdEncoding.EncodeToString(legacy)}).Fixture(plugin.OpDeviceAnnounce, map[string]any{"external_id": "ok"})
	registerMigrationSecretFixtures(h, "")
	var registry []byte
	h.On(plugin.OpFSWrite, func(req json.RawMessage) (any, error) {
		var m map[string]any
		_ = json.Unmarshal(req, &m)
		if m["path"] == "devices.json" {
			registry, _ = base64.StdEncoding.DecodeString(m["data_b64"].(string))
		}
		return map[string]any{"ok": true}, nil
	})
	h.On(plugin.OpFSRead, func(req json.RawMessage) (any, error) {
		return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(registry)}, nil
	}).Fixture(plugin.OpLegacyCommit, map[string]any{"ok": true})
	if err := migrateLegacy(&plugin.Context{}); err != nil {
		t.Fatal(err)
	}
	calls := h.Calls()
	if calls[len(calls)-1].Op != plugin.OpLegacyCommit {
		t.Fatal("commit not last")
	}
	if len(registry) == 0 || json.Valid(registry) == false {
		t.Fatal("registry")
	}
	if string(registry) == string(legacy) {
		t.Fatal("legacy copied")
	}
	if jsonContains(registry, "token_hex") {
		t.Fatal(string(registry))
	}
}
func jsonContains(b []byte, s string) bool { return len(b) > 0 && containsString(string(b), s) }
func containsString(v, s string) bool {
	for i := 0; i+len(s) <= len(v); i++ {
		if v[i:i+len(s)] == s {
			return true
		}
	}
	return false
}
