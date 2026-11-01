package main

import (
	"encoding/base64"
	"encoding/json"
	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
	"strings"
	"testing"
)

func legacyDevices() []byte {
	b, _ := json.Marshal([]legacyMIIODevice{{ExternalID: "d1", Host: "192.0.2.1", TokenHex: "00112233445566778899aabbccddeeff", Model: "m1", Capabilities: []string{"switch"}}, {ExternalID: "d2", Host: "192.0.2.2", TokenHex: "ffeeddccbbaa99887766554433221100", Model: "m2", Capabilities: []string{"light"}}})
	return b
}
func tokenHost(t *testing.T, failSecond bool) *hosttest.Host {
	h := hosttest.New(t)
	h.Fixture(plugin.OpDeviceAnnounce, map[string]any{"external_id": "ok"})
	if failSecond {
		registerMigrationSecretFixtures(h, "d2")
	} else {
		registerMigrationSecretFixtures(h, "")
	}
	h.Fixture(plugin.OpFSWrite, map[string]any{"ok": true})
	h.On(plugin.OpFSRead, func(json.RawMessage) (any, error) {
		return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString([]byte(`{"version":2,"devices":[]}`))}, nil
	})
	return h
}
func TestDeviceTokenMigrationNoTokenInRegistryRequest(t *testing.T) {
	h := tokenHost(t, false)
	err := migrateDeviceTokens(&plugin.Context{}, legacyDevices())
	if err == nil {
		t.Fatal("fixture readback intentionally mismatches")
	}
	for _, c := range h.CallsOf(plugin.OpFSWrite) {
		if strings.Contains(string(c.Request), "001122") || strings.Contains(string(c.Request), "token_hex") {
			t.Fatal(string(c.Request))
		}
	}
}
func TestDeviceTokenMigrationMidFailureNoRegistryWrite(t *testing.T) {
	h := tokenHost(t, true)
	if err := migrateDeviceTokens(&plugin.Context{}, legacyDevices()); err == nil {
		t.Fatal("want failure")
	}
	if len(h.CallsOf(plugin.OpFSWrite)) != 0 {
		t.Fatal("registry written")
	}
}
func TestDeviceTokenBadShape(t *testing.T) {
	h := hosttest.New(t)
	b, _ := json.Marshal([]legacyMIIODevice{{ExternalID: "d", Host: "h", TokenHex: "bad"}})
	if err := migrateDeviceTokens(&plugin.Context{}, b); err == nil {
		t.Fatal("want error")
	}
	if len(h.Calls()) != 0 {
		t.Fatal("host mutated")
	}
}
