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
 * device_detail：设备详情页的真实数据（2026-11-20 用户「台灯控制 传感器 配置
 * 没有数据啊：插件侧可以获取到的啊实际数据是--开关 温度 指示灯开关」）。
 *
 * # 为什么新增工具而不是塞进 device_list
 *
 * 详情数据有两层：
 *   ① 规格（控件与只读属性的**定义**）—— 查目录即可，离线也有；
 *   ② 实时值（开关是开是关、温度几度）—— 要真的去问设备。
 * ① 在列表渲染时就要（tab 标题的计数）；② 只有点开详情才值得读
 * （每台一次局域网/云端往返，塞进列表会让 /plugins 类页面白付 N 次探测）。
 *
 * # 数据从哪来（全部插件侧既有通道，不走平台）
 *
 *   · 控件/传感器定义：型号的 MIoT 规格（loadMIoTSpec）——
 *     可写属性 → 控制（bool→开关、数值→滑杆）、动作 → 按钮；
 *     只读属性 → 传感器。命名口径与 specControls 一致
 *     （规格中文名；重名加服务名前缀，台灯的两个「开关」→「开关」/「指示灯 开关」）。
 *   · 实时值：可读属性（含可写+可读）打包 `get_properties`。
 *     局域网优先（miioBatchProperties，实测 3~35ms）；无地址/token 或失败时
 *     回落云端 `miotspec/prop/get`（cloudRequestMIoTSpec，自带 12s 有界超时）——
 *     lumi.* 网关子设备只能走云端。
 *   · 两路都失败：如实报 values_source="unreachable" + 原因，
 *     控件/传感器**定义**照常返回 —— 界面显示"设备不可达"，不装作有值。
 */

type deviceDetailProp struct {
	SIID    int            `json:"siid"`
	PIID    int            `json:"piid"`
	Name    string         `json:"name"`
	Format  string         `json:"format"`
	Unit    string         `json:"unit,omitempty"`
	Access  []string       `json:"access,omitempty"`
	Range   []float64      `json:"range,omitempty"`
	Values  map[int]string `json:"values,omitempty"` // value-list 描述（枚举值 → 人话）
	Service string         `json:"service,omitempty"`
}

/*
 * specUnitSymbol 把 MIoT 规格里的**英文单位枚举**翻成界面符号。
 *
 * 2026-11-21 用户截图：「41 celsius」—— 单位不对吧。
 * 规格里 `unit` 是 `celsius` / `percentage` / `arcdegrees` 这类英文标识，
 * 直接显示就成了 "celsius"；用户要看到的是 °C / % / °。
 * 认不出的原样返回（不猜、不硬编一个可能错的中文单位）。
 */
func specUnitSymbol(u string) string {
	switch strings.ToLower(strings.TrimSpace(u)) {
	case "celsius":
		return "°C"
	case "fahrenheit":
		return "°F"
	case "kelvin":
		return "K"
	case "percentage":
		return "%"
	case "arcdegrees":
		return "°"
	case "lux":
		return "lx"
	case "decibels":
		return "dB"
	case "seconds":
		return "s"
	case "minutes":
		return "min"
	case "hours":
		return "h"
	case "days":
		return "天"
	case "watt", "watts":
		return "W"
	case "kilowatt", "kilowatts":
		return "kW"
	case "volt", "volts":
		return "V"
	case "ampere", "amperes":
		return "A"
	case "ppm":
		return "ppm"
	case "mg-per-cubic-meter":
		return "mg/m³"
	case "micrograms-per-cubic-meter":
		return "µg/m³"
	case "none", "":
		return ""
	}
	return u
}

func deviceDetailTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		DID  string `json:"did"`
		Name string `json:"name"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &in)
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
		if in.DID == "" && in.Name != "" && ds[i].Name == in.Name {
			dev = &ds[i]
			break
		}
	}
	if dev == nil {
		return nil, errors.New("设备不在本机登记里（先同步一次）")
	}
	d := *dev

	out := map[string]any{
		"did": d.ExternalID, "name": d.Name, "model": d.Model,
		"host": d.Host, "capabilities": d.Capabilities,
	}

	// —— ① 规格层：控件 + 传感器定义 ——
	typ, ok := lookupModelType(d.Model)
	if !ok {
		out["error"] = "型号不在目录中（无规格可取）"
		return json.Marshal(out)
	}
	sp, err := loadMIoTSpec(ctx, typ)
	if err != nil {
		out["error"] = "读取规格失败：" + err.Error()
		return json.Marshal(out)
	}

	type propRef struct {
		p    deviceDetailProp
		read bool
	}
	var controls []map[string]any
	var sensors []deviceDetailProp
	var readParams []map[string]any
	refOf := map[string]deviceDetailProp{}

	for _, svc := range sp.Services {
		svcName := firstNonEmptyStr(svc.Name, humanizeURN(svc.Type))
		for _, pr := range svc.Properties {
			if svc.IID == 1 || !hasAccess(pr.Access, "read") {
				continue
			}
			item := deviceDetailProp{
				SIID: svc.IID, PIID: pr.IID,
				Name:   firstNonEmptyStr(pr.Name, humanizeURN(pr.Type)),
				Format: pr.Format, Unit: specUnitSymbol(pr.Unit), Access: pr.Access,
				Service: svcName,
			}
			if len(pr.ValueRange) >= 2 {
				item.Range = pr.ValueRange
			}
			if len(pr.ValueList) > 0 {
				vl := map[int]string{}
				for _, v := range pr.ValueList {
					vl[v.Value] = v.Description
				}
				item.Values = vl
			}
			key := fmt.Sprintf("%d.%d", svc.IID, pr.IID)
			refOf[key] = item
			readParams = append(readParams, map[string]any{"did": d.ExternalID, "siid": svc.IID, "piid": pr.IID})
			if !hasAccess(pr.Access, "write") {
				sensors = append(sensors, item) // 纯只读 ⇒ 传感器
			}
		}
		for _, a := range svc.Actions {
			if svc.IID == 1 {
				continue
			}
			controls = append(controls, map[string]any{
				"capability": "button", "instance_id": actionNameOf(a.Type),
				"name":  firstNonEmptyStr(a.Name, humanizeURN(a.Type)),
				"group": svcName, "kind": "action",
			})
		}
	}
	// 可写属性 → 控件（bool→switch、数值→number），口径与 specControls 一致。
	/*
	 * 主开关（2026-11-21 用户裁定「控制应该只有开关」「指示灯开关归配置」）：
	 * MIoT 里"主开关"就是第一个可写可读的 bool 属性（属性名 `on`，通常在
	 * service 2）。它进「控制」页签；其余可写 bool（指示灯开关 / 麦克风静音 /
	 * 睡眠模式…）是**辅助开关**，进「配置」页签 —— 与 HA 的
	 * entity_category=config 一致，也与平台设备详情同一口径。
	 *
	 * 只认**第一个** `on`：台灯规格里 service 2（主开关）与 service 3（指示灯）
	 * 都叫 `on`，全当主开关会让"控制"里出现两个开关（正是用户这次指出的问题）。
	 */
	primaryTaken := false
	nameCount := map[string]int{}
	for _, svc := range sp.Services {
		if svc.IID == 1 {
			continue
		}
		svcName := firstNonEmptyStr(svc.Name, humanizeURN(svc.Type))
		for _, pr := range svc.Properties {
			if !hasAccess(pr.Access, "write") || !hasAccess(pr.Access, "read") {
				continue // 详情页只给能读到值的控件（动作按钮在上面已收）
			}
			cap := ""
			switch pr.Format {
			case "bool":
				cap = "switch"
			case "uint8", "uint16", "uint32", "int32", "float":
				cap = "number"
			default:
				continue
			}
			name := firstNonEmptyStr(pr.Name, humanizeURN(pr.Type))
			nameCount[name]++
			primary := false
			if cap == "switch" && !primaryTaken && (actionNameOf(pr.Type) == "on" || strings.TrimSpace(pr.Name) == "开关") {
				primary = true
				primaryTaken = true
			}
			controls = append(controls, map[string]any{
				"capability":  cap,
				"instance_id": fmt.Sprintf("%d.%d", svc.IID, pr.IID),
				"name":        name, "group": svcName, "kind": "prop",
				"siid": svc.IID, "piid": pr.IID, "format": pr.Format, "unit": specUnitSymbol(pr.Unit),
				"primary": primary,
			})
		}
	}
	// 重名消歧（同 specControls）：台灯的两个「开关」→「开关」/「指示灯 开关」。
	for _, c := range controls {
		if n, _ := c["name"].(string); n != "" && nameCount[n] > 1 {
			if g, _ := c["group"].(string); g != "" && g != n {
				c["name"] = g + " " + n
			}
		}
	}
	out["controls"] = controls
	out["sensors"] = sensors

	// —— ② 实时层：把可读属性真的读一遍 ——
	values := map[string]any{}
	if len(readParams) == 0 {
		out["values_source"] = "unavailable"
		out["values_error"] = "规格里没有可读属性"
		out["values"] = values
		return json.Marshal(out)
	}
	source := ""
	var readErr error
	// 局域网优先（有地址 + token）；lumi.* 是网关子设备，直连必超时，直接走云端。
	if !strings.HasPrefix(strings.ToLower(d.Model), "lumi.") &&
		strings.TrimSpace(d.Host) != "" {
		if tok, _, terr := ctx.VaultBlobOpenDevice("miio_token", d.ExternalID); terr == nil && len(tok) == 16 {
			rows, rerr := miioBatchProperties(ctx, d, tok, "get_properties", readParams)
			if rerr == nil {
				source = "lan"
				for _, row := range rows {
					if code, cok := row["code"].(float64); cok && int(code) != 0 {
						continue
					}
					siid, _ := row["siid"].(float64)
					piid, _ := row["piid"].(float64)
					if v, vok := row["value"]; vok {
						values[fmt.Sprintf("%d.%d", int(siid), int(piid))] = v
					}
				}
			} else {
				readErr = rerr
			}
		}
	}
	if source == "" {
		// 云端回落：lumi.* / 无地址 / 局域网失败都走这里（自带 12s 有界超时）。
		if sessionID, ssec, devID, oerr := openCloudSession(ctx); oerr == nil {
			rows, rerr := cloudRequestMIoTSpec(ctx, sessionID, ssec, devID, "prop/get", readParams)
			_ = ctx.HTTPClose(sessionID)
			if rerr == nil {
				source = "cloud"
				readErr = nil
				for _, row := range rows {
					if code, cok := row["code"].(float64); cok && int(code) != 0 {
						continue
					}
					siid, _ := row["siid"].(float64)
					piid, _ := row["piid"].(float64)
					if v, vok := row["value"]; vok {
						values[fmt.Sprintf("%d.%d", int(siid), int(piid))] = v
					}
				}
			} else if readErr == nil {
				readErr = rerr
			}
		} else if readErr == nil {
			readErr = oerr
		}
	}
	if source != "" {
		out["values"] = values
		out["values_source"] = source
		out["values_at"] = time.Now().UTC().Format(time.RFC3339)
	} else {
		out["values"] = values
		out["values_source"] = "unreachable"
		if readErr != nil {
			out["values_error"] = readErr.Error()
		} else {
			out["values_error"] = "无可用通道（无局域网地址/token，且云会话不可用）"
		}
	}
	return json.Marshal(out)
}
