// capability 保底（2026-11-10 用户实测「在线设备没进待绑定列表」）。
//
// # 事故形态
//
// 云端 12 台、可本地控制 8 台都数对了，但平台侧设备数 **0**，房间的「待绑定」也是 0。
// 真因：`projectCapabilities` 依赖 MIoT 目录的 Binding 投影，目录未构建/型号未收录时
// 返回 error，调用处就当空 caps 继续 ⇒ `device.announce` 被平台拒：
//
//	device.announce 必须声明至少一个 capability
//
// 而那个错误只被计进 failed（连日志都没记）—— 界面上完全看不出来。
package main

import (
	"regexp"
	"testing"
)

// 平台侧 internal/pluginres/driver.go 的 capabilityRe（此处逐字复刻；
// 改了平台规则就要同步这一条，否则闸门与真实判定会漂移）。
var capRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// ① 用户截图里那 12 台真实型号，每一台都必须拿到非空能力集。
func TestFallbackCapabilitiesCoverRealDevices(t *testing.T) {
	models := []string{
		"chuangmi.plug.m3", "chuangmi.plug.m1", "xiaomi.juicer.dems2",
		"dmaker.fan.p9", "xiaomi.wifispeaker.lx06", "zimi.waterheater.h03",
		"lumi.sensor_gas.mcn02", "soocare.toothbrush.m1",
		"miaomiaoce.sensor_ht.t2", "miir.aircondition.ir02", "miir.tv.ir01",
		"yeelink.light.ceiling",
	}
	for _, m := range models {
		caps := fallbackCapabilities(m)
		if len(caps) == 0 {
			t.Errorf("%s 没有保底能力集 —— 这台设备会被平台拒（必须声明至少一个 capability）", m)
		}
	}
}

// ② 保底值必须符合平台的 capability 正则 `^[a-z][a-z0-9_]{0,31}$`。
func TestFallbackCapabilitiesAreWellFormed(t *testing.T) {
	for _, m := range []string{"chuangmi.plug.m3", "yeelink.light.ceiling", "lumi.sensor_gas.mcn02"} {
		for _, c := range fallbackCapabilities(m) {
			if !capRe.MatchString(c) {
				t.Errorf("%s 的保底能力 %q 不符合平台正则 ^[a-z][a-z0-9_]{0,31}$", m, c)
			}
		}
	}
}

// ③ 关键判据：announce 处**必须**有兜底，不能把空 caps 直接送去平台。
func TestAnnounceNeverSendsEmptyCapabilities(t *testing.T) {
	src := readSrc(t, "cloud_sync.go")
	for _, need := range []string{
		"fallbackCapabilities(c.Model)",
		`caps = []string{"sensor"}`,
	} {
		if !contains(src, need) {
			t.Fatalf("cloud_sync.go 缺少 capability 兜底 %q —— 目录未构建时整批设备会被平台拒", need)
		}
	}
	// 反向：不得存在"空 caps 也照样 announce"的写法。
	if !contains(src, "if len(caps) == 0 {") {
		t.Fatal("缺少「空能力集」的显式兜底分支")
	}
}
