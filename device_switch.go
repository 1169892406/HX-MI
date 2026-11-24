package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"homex.dev/homex/sdk/go/plugin"
)

/*
 * device_switch：插件页里那颗开关的**下发通道**（2026-11-21 用户
 * 「控制和配置要真实可以点击的」）。
 *
 * # 为什么需要它
 *
 * 在那之前插件页是只读的：详情页把规格里的控件和实时值画出来，但点不动 ——
 * 命令只能从**平台设备页**下发（平台 → driver.command → 插件）。用户的判断很直接：
 * 一个画出来的开关点不动，就是假控件。既然写设备的能力本来就在插件里
 * （driver.command 走的就是同一套 miIO 写入），把它再开一个入口给插件自己的页面
 * 用，就不需要平台为"插件 UI 能不能控设备"新增任何东西。
 *
 * # 通道顺序（与状态读取完全一致）
 *
 *	① 局域网直连 set_properties（实测 3~35ms，且不依赖云会话是否过期）
 *	② 失败回落云端 miotspec/prop/set（Zigbee 子设备 / 不在同一网段时只有这条路）
 *	③ 两路都失败：如实报错（附两段原因），**绝不假装成功**
 *
 * # 成功之后
 *
 *   - 主开关（capability=switch 且**能力名实例**）：顺带把新状态回报给平台，
 *     平台设备页不必等下一轮同步就能看到；
 *   - 辅助开关（指示灯 / 麦克风静音…）：只回读并回给前端，不改平台状态 ——
 *     平台的状态行按能力合并，往 "switch" 里写辅助实例的值会把主开关盖掉。
 */

const deviceSwitchTimeout = 12 * time.Second

func deviceSwitchTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		DID   string `json:"did"`
		Name  string `json:"name"`
		SIID  int    `json:"siid"`
		PIID  int    `json:"piid"`
		Value bool   `json:"value"`
		// Primary 由前端告知这是不是主开关（device_detail 的 primary 字段原样带回）。
		Primary bool `json:"primary"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &in)
	}
	if in.SIID <= 0 || in.PIID <= 0 {
		return nil, errors.New("需要 siid 与 piid（来自 device_detail 的控件定义）")
	}
	if strings.TrimSpace(in.DID) == "" && strings.TrimSpace(in.Name) == "" {
		return nil, errors.New("需要 did 或 name 定位设备")
	}

	ds, err := loadDeviceRegistry(ctx)
	if err != nil {
		return nil, err
	}
	var dev *miioDevice
	for i := range ds {
		if in.DID != "" && ds[i].ExternalID == in.DID {
			dev = &ds[i]
			break
		}
		if in.DID == "" && ds[i].Name == in.Name {
			dev = &ds[i]
			break
		}
	}
	if dev == nil {
		return nil, errors.New("设备不在本机登记里（先同步一次）")
	}
	d := *dev

	started := time.Now()
	out := map[string]any{"ok": false, "did": d.ExternalID, "siid": in.SIID, "piid": in.PIID, "value": in.Value}
	params := []map[string]any{{"did": d.ExternalID, "siid": in.SIID, "piid": in.PIID, "value": in.Value}}

	/*
	 * ★★★ 老协议设备走参考项目的 set_<prop>（2026-11-24 用户「你他妈抄都抄不对吗，
	 * 核心代码你倒是看啊」）。
	 *
	 * 参考项目 miio2miot.py:180：`setter: True` ⇒ 写方法名 = `set_` + 属性名。
	 * 台灯 chuangmi.plug.m3 的真实映射（继承 m1 → hmi205）：
	 *     prop.2.1 → power     ⇒ set_power(["on"/"off"])
	 *     prop.3.1 → wifi_led  ⇒ set_wifi_led(["on"/"off"])   ← 指示灯
	 *
	 * 此前走 MIoT `set_properties` 写 3.1 会被固件当成主开关（实测写 3.1=false
	 * 把 2.1 一起关了），所以"指示灯开关坏"、且读回值永远错。
	 * 现在按表查属性名，用对应的 set_<prop> 精确下发。
	 */
	var lanErr, cloudErr error
	source := ""
	if lp := legacyPropsOf(d.Model); lp != nil {
		key := fmt.Sprintf("%d.%d", in.SIID, in.PIID)
		entry, ok := lp[key]
		if ok && entry.Setter {
			if tok, _, terr := ctx.VaultBlobOpenDevice("miio_token", d.ExternalID); terr == nil && len(tok) == 16 {
				word := "off"
				if in.Value {
					word = "on"
				} else if entry.Format == "onoff" {
					word = "off"
				} else if !in.Value {
					word = "off"
				}
				if in.Value {
					word = "on"
				}
				if _, serr := miioExchange(ctx, d, tok, "set_"+entry.Prop, []string{word}); serr == nil {
					source = "lan"
					out["legacy_prop"] = entry.Prop
					out["legacy_method"] = "set_" + entry.Prop
				} else {
					lanErr = serr
				}
			} else {
				lanErr = errors.New("设备 token 不可用")
			}
			if source != "" {
				out["ok"] = true
				out["via"] = source
				out["latency_ms"] = time.Since(started).Milliseconds()
				// 回读同一个老协议属性（get_prop），这才是设备真值。
				if tok2, _, terr := ctx.VaultBlobOpenDevice("miio_token", d.ExternalID); terr == nil {
					if got, gerr := legacyGetProp(ctx, d, tok2, []string{entry.Prop}); gerr == nil {
						if on, ok2 := legacyPowerValue(got[entry.Prop]); ok2 {
							out["read_back"] = on
						}
					}
				}
				return json.Marshal(out)
			}
		} else if ok && !entry.Setter {
			out["error"] = fmt.Sprintf("属性 %s（%s）是只读的，不能写", key, entry.Prop)
			return json.Marshal(out)
		}
	}

	/*
	 * ★★ 2026-11-21 实测（用户「台灯配置按钮和开关按钮一样的能力映射错误了」）：
	 *
	 * 台灯（chuangmi.plug.m3）写**指示灯**（siid=3/piid=1）时，走局域网
	 * `set_properties` 会被固件当成"主开关"处理 —— 实测：写 3.1=false 之后
	 * 回读 2.1 从 true 变成 false，而 3.1 立刻又回 true（设备根本没接受这次
	 * 指示灯写入，只是把整机断电了）。
	 *
	 * 云端 `miotspec/prop/set` 是按 (siid,piid) 精确路由的，不会串 —— 这也是
	 * 参考实现的写法。所以：
	 *   · 主开关（primary）→ 局域网优先（快、不吃云会话）
	 *   · 辅助开关（指示灯 / 静音 / 睡眠模式）→ **云端优先**，失败再回落局域网
	 */
	primarySwitch := in.Primary
	if !primarySwitch {
		if sessionID, ssec, devID, oerr := openCloudSession(ctx); oerr == nil {
			rows, rerr := cloudRequestMIoTSpec(ctx, sessionID, ssec, devID, "prop/set", params)
			_ = ctx.HTTPClose(sessionID)
			if rerr == nil && firstErrorCode(rows) == 0 {
				source = "cloud"
			} else if rerr != nil {
				cloudErr = rerr
			}
		} else {
			cloudErr = oerr
		}
	}

	// ① 局域网（lumi.* 是网关子设备，直连必超时，直接跳过）
	if source == "" && !strings.HasPrefix(strings.ToLower(d.Model), "lumi.") && strings.TrimSpace(d.Host) != "" {
		if tok, _, terr := ctx.VaultBlobOpenDevice("miio_token", d.ExternalID); terr == nil && len(tok) == 16 {
			rows, rerr := miioBatchProperties(ctx, d, tok, "set_properties", params)
			if rerr == nil {
				// 部分固件仍会回 code≠0（例如只读属性）—— 那算失败，不能当成功报。
				if bad := firstErrorCode(rows); bad != 0 {
					lanErr = fmt.Errorf("设备返回 code=%d", bad)
				} else {
					source = "lan"
				}
			} else {
				lanErr = rerr
			}
		} else {
			lanErr = errors.New("设备 token 不可用")
		}
	} else if strings.HasPrefix(strings.ToLower(d.Model), "lumi.") {
		lanErr = errors.New("网关子设备不走局域网直连")
	} else {
		lanErr = errors.New("无局域网地址")
	}

	// ② 云端回落
	if source == "" {
		if sessionID, ssec, devID, oerr := openCloudSession(ctx); oerr == nil {
			rows, rerr := cloudRequestMIoTSpec(ctx, sessionID, ssec, devID, "prop/set", params)
			_ = ctx.HTTPClose(sessionID)
			if rerr == nil {
				if bad := firstErrorCode(rows); bad != 0 {
					cloudErr = fmt.Errorf("设备返回 code=%d", bad)
				} else {
					source = "cloud"
				}
			} else {
				cloudErr = rerr
			}
		} else {
			cloudErr = oerr
		}
	}

	if source == "" {
		out["error"] = fmt.Sprintf("下发失败：局域网 %v；云端 %v", briefErr(lanErr), briefErr(cloudErr))
		return json.Marshal(out)
	}

	out["ok"] = true
	out["via"] = source
	out["latency_ms"] = time.Since(started).Milliseconds()

	// 回读确认（写成功不等于值真的变了：部分固件接受写入但忽略非法值）。
	readParams := []map[string]any{{"did": d.ExternalID, "siid": in.SIID, "piid": in.PIID}}
	if got, ok := readBackBool(ctx, d, in.SIID, in.PIID, readParams); ok {
		out["read_back"] = got
	}

	/*
	 * 主开关才回报平台状态。
	 *
	 * 平台状态行按**能力**合并（同 capability 的 value 覆盖），而辅助开关的实例名
	 * （indicator_light_on / microphone_mute…）在平台的控件侧是另一个实例、状态侧却
	 * 只有一个 `switch` 槽 —— 往那里写辅助值会把主开关的状态盖掉。
	 * 所以：主开关 → 回报；辅助开关 → 只回给前端（界面自己刷新）。
	 */
	if in.Primary {
		if perr := ctx.DeviceStatePublish(plugin.DeviceStateRequest{
			ExternalID: d.ExternalID,
			State:      map[string]any{"switch": map[string]any{"power": in.Value}},
			ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}); perr != nil {
			ctx.LogWarn("hx-mi 开关状态回报失败（device=%s）：%v", d.ExternalID, perr)
		}
	}
	return json.Marshal(out)
}

// firstErrorCode 取批量结果里第一个非零 code（0 或缺失 = 成功）。
func firstErrorCode(rows []map[string]any) int {
	for _, row := range rows {
		if code, ok := row["code"].(float64); ok && int(code) != 0 {
			return int(code)
		}
	}
	return 0
}

// readBackBool 写后回读一次（同一通道，局域网优先）。读不到就不给结论。
// pickBoolAt 从返回行里挑出**指定 (siid,piid)** 的 bool 值。
//
// # 为什么必须按 siid/piid 精确匹配（2026-11-23，用户「指示灯关不掉/状态不变」根因）
//
// 原实现是「遍历返回行，取第一个 bool」：
//
//	for _, row := range rows {
//	    if b, ok := row["value"].(bool); ok { return b, true }
//	}
//
// 设备有多路可写开关时（台灯 chuangmi.plug.m3：service 2 = 主开关、service 3 =
// 指示灯），get_properties 一起回来的行里**第一个 bool 往往是主开关**。于是：
//
//	写 3.1=false（关指示灯）→ 设备真实变成 {'2.1':true,'3.1':false}（写入成功）
//	                          → read_back 却读到 2.1=true
//	                          → 界面把开关画成「开」⇒ 用户点了没反应
//
// 实测（服务器，alpha.199）：连写 false/true/false，read_back 恒为 True，
// 而 device_detail 的 values 显示 3.1 确实在变 —— 写入没问题，是回读读错了路。
//
// 兜底：行里没有 siid/piid（个别固件/网关不回这两个字段）时，只在**只有一行**
// 的情况下采用 —— 多行而又无从区分时宁可报"读不确定"，让前端用期望值，
// 也不要拿另一路的状态冒充这一路。
func pickBoolAt(rows []map[string]any, siid, piid int) (bool, bool) {
	var only map[string]any
	for _, row := range rows {
		b, ok := row["value"].(bool)
		if !ok {
			continue
		}
		rs, rok := intField(row, "siid")
		rp, pok := intField(row, "piid")
		if rok && pok {
			if rs == siid && rp == piid {
				return b, true
			}
			continue // 明确属于别的属性：跳过，绝不能拿来冒充
		}
		if only == nil {
			only = row
		} else {
			only = row // 多行都缺 siid/piid：无法区分，置为不可判定
			return false, false
		}
	}
	if only != nil {
		if b, ok := only["value"].(bool); ok {
			return b, true
		}
	}
	return false, false
}

// intField 从返回行里取整数字段（JSON 数字常解成 float64，也可能是字符串）。
func intField(row map[string]any, key string) (int, bool) {
	switch v := row[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

func readBackBool(ctx *plugin.Context, d miioDevice, siid, piid int,
	params []map[string]any) (bool, bool) {
	if !strings.HasPrefix(strings.ToLower(d.Model), "lumi.") && strings.TrimSpace(d.Host) != "" {
		if tok, _, terr := ctx.VaultBlobOpenDevice("miio_token", d.ExternalID); terr == nil && len(tok) == 16 {
			if rows, e := miioBatchProperties(ctx, d, tok, "get_properties", params); e == nil {
				if b, ok := pickBoolAt(rows, siid, piid); ok {
					return b, true
				}
			}
		}
	}
	if sessionID, ssec, devID, e := openCloudSession(ctx); e == nil {
		rows, rerr := cloudRequestMIoTSpec(ctx, sessionID, ssec, devID, "prop/get", params)
		_ = ctx.HTTPClose(sessionID)
		if rerr == nil {
			if b, ok := pickBoolAt(rows, siid, piid); ok {
				return b, true
			}
		}
	}
	return false, false
}

// briefErr 把可能为 nil 的错误压成一句短话（本文件专用，避免与 lan_health 的 shortErr 撞名）。
func briefErr(e error) string {
	if e == nil {
		return "未尝试"
	}
	msg := e.Error()
	if len(msg) > 90 {
		msg = msg[:90] + "…"
	}
	return msg
}

/*
 * device_control：规格驱动的**动作 / 数值**下发（2026-11-21 用户：
 * 「所有设备在插件侧都应该可以用，后期映射能力到平台啊，插件侧都不行怎么映射到平台呢」）。
 *
 * 与 device_switch 的分工：
 *   · device_switch 只管 bool 属性（开关类），语义最窄、最不容易误写；
 *   · device_control 管其余两类规格驱动能力：
 *       press      → 调规格里的动作（siid + aiid，`in` 为空）
 *       set_value  → 写数值属性（siid + piid + value）
 *
 * 通道顺序同样是 局域网优先 → 云端回落（动作两类都支持）。
 * 这里不解码动作返回值：action 的 out 参数各型号差异极大，编一套通用解码必然是错的；
 * 界面上「已下发」代表设备接受了这次调用，不是"已达成你期望的物理效果"。
 */
func deviceControlTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		DID   string `json:"did"`
		Name  string `json:"name"`
		Kind  string `json:"kind"` // press | set_value
		SIID  int    `json:"siid"`
		PIID  int    `json:"piid"`
		AIID  int    `json:"aiid"`
		Value any    `json:"value"`
		Label string `json:"label"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &in)
	}
	if in.SIID <= 0 {
		return nil, errors.New("需要 siid（来自 device_detail 的控件定义）")
	}
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if kind == "" {
		kind = "press"
	}
	if kind == "press" && in.AIID <= 0 {
		return nil, errors.New("press 需要 aiid（动作 id）")
	}
	if kind == "set_value" && in.PIID <= 0 {
		return nil, errors.New("set_value 需要 piid（属性 id）")
	}

	ds, err := loadDeviceRegistry(ctx)
	if err != nil {
		return nil, err
	}
	var dev *miioDevice
	for i := range ds {
		if in.DID != "" && ds[i].ExternalID == in.DID {
			dev = &ds[i]
			break
		}
		if in.DID == "" && in.Name != "" && ds[i].Name == in.Name {
			dev = &ds[i]
			break
		}
	}
	if dev == nil {
		return nil, errors.New("设备不在本机登记里（先同步一次）")
	}
	d := *dev
	started := time.Now()
	out := map[string]any{"ok": false, "did": d.ExternalID, "kind": kind}

	var lanErr, cloudErr error
	source := ""

	// ① 局域网
	if !strings.HasPrefix(strings.ToLower(d.Model), "lumi.") && strings.TrimSpace(d.Host) != "" {
		if tok, _, terr := ctx.VaultBlobOpenDevice("miio_token", d.ExternalID); terr == nil && len(tok) == 16 {
			if kind == "press" {
				rows, rerr := miioBatchProperties(ctx, d, tok, "action",
					[]map[string]any{{"did": d.ExternalID, "siid": in.SIID, "aiid": in.AIID, "in": []any{}}})
				if rerr == nil && firstErrorCode(rows) == 0 {
					source = "lan"
				} else if rerr != nil {
					lanErr = rerr
				}
			} else {
				rows, rerr := miioBatchProperties(ctx, d, tok, "set_properties",
					[]map[string]any{{"did": d.ExternalID, "siid": in.SIID, "piid": in.PIID, "value": in.Value}})
				if rerr == nil && firstErrorCode(rows) == 0 {
					source = "lan"
				} else if rerr != nil {
					lanErr = rerr
				}
			}
		} else {
			lanErr = errors.New("设备 token 不可用")
		}
	} else {
		lanErr = errors.New("无局域网地址（走云端）")
	}

	// ② 云端回落
	if source == "" {
		if sessionID, ssec, devID, oerr := openCloudSession(ctx); oerr == nil {
			var rows []map[string]any
			var rerr error
			if kind == "press" {
				rows, rerr = cloudRequestMIoTSpec(ctx, sessionID, ssec, devID, "action",
					map[string]any{"did": d.ExternalID, "siid": in.SIID, "aiid": in.AIID, "in": []any{}})
			} else {
				rows, rerr = cloudRequestMIoTSpec(ctx, sessionID, ssec, devID, "prop/set",
					[]map[string]any{{"did": d.ExternalID, "siid": in.SIID, "piid": in.PIID, "value": in.Value}})
			}
			_ = ctx.HTTPClose(sessionID)
			if rerr == nil && firstErrorCode(rows) == 0 {
				source = "cloud"
			} else if rerr != nil {
				cloudErr = rerr
			}
		} else {
			cloudErr = oerr
		}
	}

	if source == "" {
		out["error"] = fmt.Sprintf("下发失败：局域网 %v；云端 %v", briefErr(lanErr), briefErr(cloudErr))
		return json.Marshal(out)
	}
	out["ok"] = true
	out["via"] = source
	out["latency_ms"] = time.Since(started).Milliseconds()
	if kind == "set_value" {
		out["value"] = in.Value
	}
	return json.Marshal(out)
}
