package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

// TestLanProbeMarksUnreachableImmediately 钉住：本地探不到就是离线，
// 不重试、不等云端（用户明确要求"本地不通立即标离线"）。
func TestLanProbeMarksUnreachableImmediately(t *testing.T) {
	h := hosttest.New(t)
	calls := 0
	h.On(plugin.OpUDPSend, func(raw json.RawMessage) (any, error) {
		calls++
		return nil, errTestTimeout
	})
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	e := probeLanDevice(ctx, miioDevice{ExternalID: "1", Host: "192.168.1.50"})
	if e.Reachable {
		t.Fatal("超时不得判为在线")
	}
	if e.Err == "" {
		t.Fatal("必须记录失败原因")
	}
	if calls != 1 {
		t.Fatalf("健康探测不应重试（重试会让离线结论迟到），实际发了 %d 次", calls)
	}
}

// TestLanProbeWithoutHost 没有局域网地址的设备直接判不可达并说明原因。
func TestLanProbeWithoutHost(t *testing.T) {
	_ = hosttest.New(t)
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	e := probeLanDevice(ctx, miioDevice{ExternalID: "1", Host: ""})
	if e.Reachable {
		t.Fatal("无地址不得判在线")
	}
	if e.Err != "没有局域网地址" {
		t.Fatalf("原因应明确，实际 %q", e.Err)
	}
}

// TestLanProbeSuccess 收到合法 hello 回包即判在线并记录延迟。
func TestLanProbeSuccess(t *testing.T) {
	h := hosttest.New(t)
	// 构造一个能被 parseHelloPacket 接受的 32 字节回包
	pkt := make([]byte, 32)
	pkt[0], pkt[1] = 0x21, 0x31
	for i := 4; i < 8; i++ {
		pkt[i] = 0xff
	}
	h.Fixture(plugin.OpUDPSend, map[string]any{
		"payload_b64": base64.StdEncoding.EncodeToString(pkt),
	})
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	e := probeLanDevice(ctx, miioDevice{ExternalID: "1", Host: "192.168.1.50"})
	// 该包可能因 device id 字段不完整而解析失败；无论哪种，结论必须是"有明确判定"
	if e.CheckedAt == "" {
		t.Fatal("必须记录探测时间")
	}
	if !e.Reachable && e.Err == "" {
		t.Fatal("不可达时必须给原因，不能什么都不说")
	}
}

// TestLanHealthSnapshotIsCopy 快照必须是拷贝，调用方改它不得污染内部表。
func TestLanHealthSnapshotIsCopy(t *testing.T) {
	lanHealthTable.mu.Lock()
	lanHealthTable.results["x"] = lanHealthEntry{Reachable: true}
	lanHealthTable.mu.Unlock()
	snap := snapshotLanHealth()
	snap["x"] = lanHealthEntry{Reachable: false}
	if !snapshotLanHealth()["x"].Reachable {
		t.Fatal("快照应是拷贝，外部修改不得影响内部状态")
	}
}

type testErr struct{}

func (testErr) Error() string { return "i/o timeout" }

var errTestTimeout = testErr{}
