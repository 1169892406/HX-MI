package main

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

// buildHello 造一个 device_id 可控的 miio hello 回包。
func buildHello(devID uint32) []byte {
	pkt := make([]byte, 32)
	pkt[0], pkt[1] = 0x21, 0x31
	binary.BigEndian.PutUint32(pkt[4:8], 0) // stamp
	binary.BigEndian.PutUint32(pkt[8:12], devID)
	for i := 12; i < 32; i++ {
		pkt[i] = 0xff
	}
	return pkt
}

// TestLanProbeRejectsMismatchedDeviceID 钉住：IP 上应答的不是我们登记的那台设备时，
// 必须判为地址失效，不能算作在线。
//
// 实测事故：登记表里「风扇」与「天然气卫士」同为 192.168.1.58，该地址实际应答的
// device_id 与两者都不符（云端 localip 已过期）。只看到回包就判在线，会把陌生设备
// 的应答算到天然气卫士头上 —— 界面显示在线，点它却没反应。
func TestLanProbeRejectsMismatchedDeviceID(t *testing.T) {
	h := hosttest.New(t)
	// 设备实际应答 id=1035850024，而我们登记的是 684834109
	h.Fixture(plugin.OpUDPSend, map[string]any{
		"payload_b64": base64.StdEncoding.EncodeToString(buildHello(1035850024)),
	})
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	e := probeLanDevice(ctx, miioDevice{
		ExternalID: "684834109", NumericDeviceID: 684834109, Host: "192.168.1.58",
	})
	if e.Reachable {
		t.Fatal("device_id 不匹配时不得判在线（那是另一台设备在应答）")
	}
	if !strings.Contains(e.Err, "地址不符") {
		t.Fatalf("原因应指明地址已过期，实际 %q", e.Err)
	}
}

// TestLanProbeAcceptsMatchingDeviceID 匹配时必须判在线。
func TestLanProbeAcceptsMatchingDeviceID(t *testing.T) {
	h := hosttest.New(t)
	h.Fixture(plugin.OpUDPSend, map[string]any{
		"payload_b64": base64.StdEncoding.EncodeToString(buildHello(684834109)),
	})
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	e := probeLanDevice(ctx, miioDevice{
		ExternalID: "684834109", NumericDeviceID: 684834109, Host: "192.168.1.58",
	})
	if !e.Reachable {
		t.Fatalf("device_id 匹配应判在线，实际 err=%q", e.Err)
	}
}

// TestLanProbeAllowsUnknownDeviceID 未登记 numeric id 时不做校验（避免误杀）。
func TestLanProbeAllowsUnknownDeviceID(t *testing.T) {
	h := hosttest.New(t)
	h.Fixture(plugin.OpUDPSend, map[string]any{
		"payload_b64": base64.StdEncoding.EncodeToString(buildHello(999)),
	})
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	e := probeLanDevice(ctx, miioDevice{ExternalID: "x", NumericDeviceID: 0, Host: "192.168.1.9"})
	if !e.Reachable {
		t.Fatalf("未登记 numeric id 时不应校验 device_id，实际 %q", e.Err)
	}
}
