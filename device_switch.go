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

	var lanErr, cloudErr error
	source := ""

	// ① 局域网（lumi.* 是网关子设备，直连必超时，直接跳过）
	if !strings.HasPrefix(strings.ToLower(d.Model), "lumi.") && strings.TrimSpace(d.Host) != "" {
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
func readBackBool(ctx *plugin.Context, d miioDevice, siid, piid int,
	params []map[string]any) (bool, bool) {
	if !strings.HasPrefix(strings.ToLower(d.Model), "lumi.") && strings.TrimSpace(d.Host) != "" {
		if tok, _, terr := ctx.VaultBlobOpenDevice("miio_token", d.ExternalID); terr == nil && len(tok) == 16 {
			if rows, e := miioBatchProperties(ctx, d, tok, "get_properties", params); e == nil {
				for _, row := range rows {
					if b, ok := row["value"].(bool); ok {
						return b, true
					}
				}
			}
		}
	}
	if sessionID, ssec, devID, e := openCloudSession(ctx); e == nil {
		rows, rerr := cloudRequestMIoTSpec(ctx, sessionID, ssec, devID, "prop/get", params)
		_ = ctx.HTTPClose(sessionID)
		if rerr == nil {
			for _, row := range rows {
				if b, ok := row["value"].(bool); ok {
					return b, true
				}
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
