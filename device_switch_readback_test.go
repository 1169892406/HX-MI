package main

import "testing"

// 回读必须按 (siid,piid) 精确匹配（2026-11-23）。
//
// 用户实测：「指示灯开关关闭了无法打开，开关状态也不变」。
// 服务器实测：连写 false/true/false，read_back 恒为 True，而 device_detail 的
// values 显示 3.1 确实在变 —— 写入成功，是**回读读错了路**：
// 台灯有两路可写开关（2.1 主开关 / 3.1 指示灯），旧实现「取第一个 bool」
// 拿到的是主开关 true，于是界面永远画成「开」。
func TestPickBoolMatchesExactInstance(t *testing.T) {
	rows := []map[string]any{
		{"siid": float64(2), "piid": float64(1), "value": true},  // 主开关
		{"siid": float64(3), "piid": float64(1), "value": false}, // 指示灯
	}
	// 查指示灯：必须拿到 false，绝不能拿主开关的 true
	if b, ok := pickBoolAt(rows, 3, 1); !ok || b != false {
		t.Fatalf("指示灯回读 = (%v,%v)（期望 false,true）—— 读成主开关就是用户看到的「状态不变」", b, ok)
	}
	// 查主开关：仍然是 true
	if b, ok := pickBoolAt(rows, 2, 1); !ok || b != true {
		t.Fatalf("主开关回读 = (%v,%v)（期望 true,true）", b, ok)
	}
	// 查不存在的属性：必须"读不到"，不能拿别的属性冒充
	if b, ok := pickBoolAt(rows, 9, 9); ok {
		t.Fatalf("不存在的 9.9 竟回读出 %v —— 拿别的属性冒充会让界面显示假状态", b)
	}
}

// 行里缺 siid/piid（个别固件不回）时：单行可采用，多行不可判定。
func TestPickBoolWithoutInstanceFields(t *testing.T) {
	if b, ok := pickBoolAt([]map[string]any{{"value": false}}, 3, 1); !ok || b {
		t.Fatalf("单行缺 siid/piid 应采用该行：(%v,%v)", b, ok)
	}
	multi := []map[string]any{{"value": true}, {"value": false}}
	if _, ok := pickBoolAt(multi, 3, 1); ok {
		t.Fatal("多行且都缺 siid/piid 时无法区分，必须报不可判定")
	}
}
