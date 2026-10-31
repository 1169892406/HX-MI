package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

const testSsecurity = "QUFBQUFBQUFBQUFBQUFBQQ==" // base64(16 zero bytes)

func TestSyncCloudDevicesMergesByDID(t *testing.T) {
	h := hosttest.New(t)
	var written []byte
	sealed := 0
	var announceCalled int
	h.On(plugin.OpFSRead, func(raw json.RawMessage) (any, error) {
		var r struct{ Path string `json:"path"` }
		_ = json.Unmarshal(raw, &r)
		switch r.Path {
		case "cloud-session.json":
			b, _ := json.Marshal(savedCloudSession{Version: 2, SecretRef: "xiaomi_session_material", SecretVersion: 1, TicketsSealed: true})
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(b)}, nil
		case "devices.json":
			b, _ := json.Marshal(deviceRegistryV2{Version: 2, Devices: []miioDevice{{ExternalID: "999", Host: "192.168.1.9", TokenRef: "miio_token"}}})
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(b)}, nil
		}
		return map[string]any{"found": false}, nil
	})
	h.On(plugin.OpFSWrite, func(raw json.RawMessage) (any, error) {
		var w struct{ Path string `json:"path"`; DataB64 string `json:"data_b64"` }
		_ = json.Unmarshal(raw, &w)
		if w.Path == "devices.json" {
			written, _ = base64.StdEncoding.DecodeString(w.DataB64)
		}
		return map[string]any{"ok": true}, nil
	})
	h.Fixture(plugin.OpVaultBlobOpen, map[string]any{"key": "xiaomi_session_material", "data_b64": base64.StdEncoding.EncodeToString([]byte(`{"ssecurity":"` + testSsecurity + `"}`)), "version": 1})
	h.Fixture(plugin.OpHTTPOpen, map[string]any{"session_id": "hs-test"})
	h.Fixture(plugin.OpHTTPCookiesAttach, map[string]any{"host": "api.io.mi.com", "attached": []map[string]any{{"name": "serviceToken"}}})
	h.Fixture(plugin.OpHTTPClose, map[string]any{"closed": true})
	h.Fixture(plugin.OpDeviceAnnounce, map[string]any{"external_id": "ok"})
	h.On(plugin.OpDeviceAnnounce, func(json.RawMessage) (any, error) { announceCalled++; return map[string]any{"external_id": "ok"}, nil })
	h.On(plugin.OpVaultBlobSeal, func(raw json.RawMessage) (any, error) {
		var in struct{ Key string `json:"key"`; DeviceID string `json:"device_id"`; DataB64 string `json:"data_b64"` }
		_ = json.Unmarshal(raw, &in)
		if in.Key == "miio_token" && in.DeviceID != "" {
			sealed++
		}
		return map[string]any{"key": in.Key, "scope": "device", "present": true, "version": 1, "size": 16}, nil
	})
	h.On(plugin.OpHTTPRequest, func(json.RawMessage) (any, error) { t.Fatal("不应走 guest HTTP"); return nil, nil })
	h.On(plugin.OpLog, func(raw json.RawMessage) (any, error) { t.Logf("plugin log: %s", string(raw)); return map[string]any{"ok": true}, nil })
	h.On(plugin.OpHTTPSessionRequest, func(raw json.RawMessage) (any, error) {
		var req plugin.HTTPSessionRequest
		_ = json.Unmarshal(raw, &req)
		body, _ := base64.StdEncoding.DecodeString(req.BodyB64)
		q, _ := url.ParseQuery(string(body))
		nonce := q.Get("_nonce")
		signed, err := signedNonceKey2(testSsecurity, nonce)
		if err != nil {
			t.Fatal(err)
		}
		key, err := base64.StdEncoding.DecodeString(signed)
		if err != nil {
			t.Fatal(err)
		}
		plain := []byte(`{"code":0,"result":{"list":[{"did":"237970870","token":"00112233445566778899aabbccddeeff","localip":"192.168.1.10","name":"Lamp","model":"unknown.model.x","isOnline":true}]}}`)
		cipher := rc4Crypt(key, plain)
		return plugin.HTTPSessionResponse{Status: 200, BodyB64: base64.StdEncoding.EncodeToString([]byte(base64.StdEncoding.EncodeToString(cipher)))}, nil
	})

	sum, err := syncCloudDevices(&plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if sum["cloud"] != 1 || sum["added"] != 1 || sum["announced"] != 1 || sum["tokens"] != 1 {
		t.Fatalf("summary=%v", sum)
	}
	if announceCalled != 1 || sealed != 1 {
		t.Fatalf("announce=%d seal=%d", announceCalled, sealed)
	}
	var reg deviceRegistryV2
	if err := json.Unmarshal(written, &reg); err != nil {
		t.Fatal(err)
	}
	if reg.Version != 3 || len(reg.Devices) != 2 {
		t.Fatalf("registry version=%d devices=%d", reg.Version, len(reg.Devices))
	}
	var got *miioDevice
	for i := range reg.Devices {
		if reg.Devices[i].ExternalID == "237970870" {
			got = &reg.Devices[i]
		}
	}
	if got == nil || got.NumericDeviceID != 237970870 || got.Host != "192.168.1.10" || got.TokenRef != "miio_token" {
		t.Fatalf("merged device=%+v", got)
	}
	fmt.Println("summary", sum)
}
