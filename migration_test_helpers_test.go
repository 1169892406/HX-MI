package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

func registerMigrationSecretFixtures(h *hosttest.Host, failDevice string) {
	sealed := map[string][]byte{}
	h.On(plugin.OpLegacyDeviceSecretSeal, func(req json.RawMessage) (any, error) {
		var m map[string]string
		_ = json.Unmarshal(req, &m)
		if m["device_id"] == failDevice {
			return nil, errors.New("injected")
		}
		b, _ := base64.StdEncoding.DecodeString(m["data_b64"])
		sealed[m["device_id"]] = b
		sum := sha256.Sum256(b)
		return plugin.LegacyDeviceSecretResponse{Source: m["source"], LogicalKey: m["logical_key"], Device: m["device_id"], Presence: true, Version: 1, Size: len(b), SHA256: fmt.Sprintf("%x", sum[:])}, nil
	})
	h.On(plugin.OpLegacyDeviceSecretInfo, func(req json.RawMessage) (any, error) {
		var m map[string]string
		_ = json.Unmarshal(req, &m)
		b := sealed[m["device_id"]]
		sum := sha256.Sum256(b)
		return plugin.LegacyDeviceSecretResponse{Source: m["source"], LogicalKey: m["logical_key"], Device: m["device_id"], Presence: len(b) > 0, Version: 1, Size: len(b), SHA256: fmt.Sprintf("%x", sum[:])}, nil
	})
	h.Fixture(plugin.OpLegacyAbort, map[string]any{"aborted": true})
}
