package main

import "testing"

// 老协议属性表必须与参考项目 hass-xiaomi-miot 逐字一致（2026-11-24）。
//
// 用户原话：「插座指示灯本身就可以关闭开启的，现在显示的是未知肯定不对啊」
//
//	「你他妈抄都抄不对吗，核心代码你倒是看啊」。
//
// 我此前两次靠"云端 MIoT spec 反推"都错了，这里把参考项目的真实映射钉死。
func TestLegacyProps_LampM3(t *testing.T) {
	lp := legacyPropsOf("chuangmi.plug.m3")
	if lp == nil {
		t.Fatal("chuangmi.plug.m3 必须有老协议属性表（继承链 m3 → m1 → hmi205）")
	}
	// 参考项目 miio2miot_specs.py:218-242：
	//   hmi205: prop.2.1 power(setter) / prop.2.2 temperature
	//   m1:     prop.3.1 wifi_led(setter)
	want := map[string]struct {
		prop   string
		setter bool
	}{
		"2.1": {"power", true},
		"2.2": {"temperature", false},
		"3.1": {"wifi_led", true}, // ← 指示灯：可读可写，此前被我误判成"不存在"
	}
	for key, w := range want {
		e, ok := lp[key]
		if !ok {
			t.Fatalf("缺属性 %s（参考项目里有，映射为 %s）", key, w.prop)
		}
		if e.Prop != w.prop {
			t.Errorf("%s 属性名 = %q，参考项目是 %q", key, e.Prop, w.prop)
		}
		if e.Setter != w.setter {
			t.Errorf("%s setter = %v，参考项目是 %v", key, e.Setter, w.setter)
		}
	}
}

// 指示灯必须可写——这是"插座指示灯打不开"的直接判据。
func TestLegacyProps_IndicatorIsWritable(t *testing.T) {
	lp := legacyPropsOf("chuangmi.plug.m3")
	if lp == nil {
		t.Fatal("缺表")
	}
	e, ok := lp["3.1"]
	if !ok || !e.Setter {
		t.Fatal("3.1 指示灯必须存在且可写（set_wifi_led），否则用户点了没反应")
	}
}

// 温度是只读的，不能被当成开关写。
func TestLegacyProps_TemperatureReadOnly(t *testing.T) {
	lp := legacyPropsOf("chuangmi.plug.m3")
	if lp == nil {
		t.Fatal("缺表")
	}
	if e, ok := lp["2.2"]; ok && e.Setter {
		t.Fatal("2.2 温度是只读属性，setter 必须为 false")
	}
}

// 表覆盖率：参考项目登记了 242 个型号，不能漏掉大半。
func TestLegacyProps_TableCoverage(t *testing.T) {
	if n := len(legacyPropTable); n < 200 {
		t.Fatalf("老协议表只有 %d 个型号，参考项目是 242 个 —— 生成时漏了", n)
	}
	total := 0
	for _, m := range legacyPropTable {
		total += len(m)
	}
	if total < 1500 {
		t.Fatalf("属性映射只有 %d 条，参考项目约 1698 条 —— 生成时漏了", total)
	}
}
