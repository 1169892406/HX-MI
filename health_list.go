package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"homex.dev/homex/sdk/go/plugin"
)

/*
 * 设备健康检查列表（2026-11-17 用户要求）。
 *
 * 列：设备 / 型号 / 版本 / 状态 / 耗时（日志列已按用户要求移除）。
 * 路径信息放在"设备"副标题里（如「网关读取 · Zigbee 经网关」）。
 *
 * # 三条路径，三种测法（关键：不能拿"有没有 IP"当唯一判据）
 *
 *	① 局域网直连（有独立 IP 的 Wi-Fi 设备）
 *	   发 UDP hello 包，3 秒超时。这是最快也最真实的一条。
 *
 *	② 网关读取（蓝牙 / Zigbee 设备，如天然气卫士、温湿度计）
 *	   **它们的数据是从音箱（网关）取的** —— 用户明确指出这一点，
 *	   所以这条路径必须被检测，而不是标"不适用"。
 *	   测法：调 `miIO.info` 到网关（音箱），确认网关链路本身活着。
 *	   设备本身不联网，网关活着 = 该设备可读。
 *
 *	③ 云端下发（红外遥控，如空调/电视遥控）
 *	   红外是单向发射、无可回读状态，无法用探测判断"可控"。
 *	   如实标「云端可用」：含义是"需要登录有效的账号才能用"，
 *	   与"离线"（故障）区分开。
 */

// 设备通信路径
const (
	pathLAN    = "局域网直连"
	pathGateway = "网关读取"
	pathCloud  = "云端下发"
)

// 健康检查的三种状态
const (
	healthOnline = "在线"
	healthOffline = "离线"
	healthCloudOK = "云端可用"
)

// healthEntry 是列表里的一行。
type healthEntry struct {
	Name       string `json:"name"`
	Model      string `json:"model"`
	Version    string `json:"version"`    // 取不到时为空，前端显示 "-"
	Path       string `json:"path"`
	PathDetail string `json:"path_detail"` // 副标题补充（IP / 经哪台网关）
	Status     string `json:"status"`
	LatencyMS  int    `json:"latency_ms"` // <0 表示不适用
	Latency    string `json:"latency"`    // 给人看的：35 ms / 3s 超时 / 不适用
}

// gwModelPrefixes 是"经网关通信、自身不联网"的型号前缀。
//
//	lumi.*        Zigbee（如 lumi.sensor_gas.mcn02 天然气卫士）
//	blt.*         BLE
//	miaomiaoce.*  蓝牙（如温湿度计 miaomiaoce.sensor_ht.t2）
//	miir.*        红外遥控
func isGatewayDevice(model string) bool {
	m := strings.ToLower(model)
	for _, p := range []string{"lumi.", "blt.", "miaomiaoce.", "hmpace.", "cgllc.", "zhimi."} {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}

func isIRDevice(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "miir.")
}

// healthListTool 生成健康检查列表（`health_list`）。
func healthListTool(ctx *plugin.Context) (json.RawMessage, error) {
	ds, err := loadDeviceRegistry(ctx)
	if err != nil {
		return nil, err
	}
	// 网关 = 有局域网地址的音箱；网关读取路径要靠它
	var gw *miioDevice
	for i := range ds {
		if strings.Contains(strings.ToLower(ds[i].Model), "speaker") && strings.TrimSpace(ds[i].Host) != "" {
			gw = &ds[i]
			break
		}
	}
	var gwToken []byte
	if gw != nil {
		if tok, _, terr := ctx.VaultBlobOpenDevice("miio_token", gw.ExternalID); terr == nil {
			gwToken = tok
		}
	}

	// 版本号：尽量从设备信息取（缓存在 registry 里；取不到就留空）
	out := make([]healthEntry, 0, len(ds))
	for _, d := range ds {
		e := healthEntry{
			Name: d.Name, Model: d.Model, Version: "",
			LatencyMS: -1, Latency: "不适用",
		}
		switch {
		case isIRDevice(d.Model):
			e.Path = pathCloud
			e.PathDetail = "云端下发 · 红外经音箱"
			e.Status = healthCloudOK
			out = append(out, e)

		case isGatewayDevice(d.Model) || strings.TrimSpace(d.Host) == "":
			e.Path = pathGateway
			if gw != nil {
				e.PathDetail = "网关读取 · 经 " + gw.Name
			} else {
				e.PathDetail = "网关读取"
			}
			// 测网关链路（3s）：网关活着 ⇒ 该设备可读
			if gw == nil || len(gwToken) != 16 {
				e.Status = healthOffline
				e.PathDetail += "（未找到可用网关）"
			} else {
				start := time.Now()
				_, perr := miioExchangeWith(ctx, *gw, gwToken, "miIO.info", []any{},
					miioExchangeOptions{Attempts: 1})
				ms := int(time.Since(start).Milliseconds())
				if perr != nil {
					e.Status = healthOffline
					e.Latency = "3s 超时"
				} else {
					e.Status = healthOnline
					e.LatencyMS = ms
					e.Latency = fmt.Sprintf("%d ms", ms)
				}
			}
			out = append(out, e)

		default:
			e.Path = pathLAN
			e.PathDetail = "局域网 " + d.Host
			start := time.Now()
			res := probeLanDevice(ctx, d)
			ms := int(time.Since(start).Milliseconds())
			if res.Reachable {
				e.Status = healthOnline
				e.LatencyMS = ms
				e.Latency = fmt.Sprintf("%d ms", ms)
				// 版本：局域网可达时顺手取（失败不影响状态判定）
				if tok, _, terr := ctx.VaultBlobOpenDevice("miio_token", d.ExternalID); terr == nil && len(tok) == 16 {
					if info, ierr := miioExchangeWith(ctx, d, tok, "miIO.info", []any{},
						miioExchangeOptions{Attempts: 1}); ierr == nil {
						e.Version = fwVersionOf(info)
					}
				}
			} else {
				e.Status = healthOffline
				e.Latency = "3s 超时"
			}
			out = append(out, e)
		}
	}
	return json.Marshal(map[string]any{
		"devices": out,
		"count":   len(out),
		"checked_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// deviceInfoTool 是**只读**诊断（`device_info`）：dump 一台设备的 miIO.info 原始字段。
//
// 用途：健康检查表想显示固件版本，但各型号字段名不统一（fw_ver / fw_version /
// application_version…）。与其猜，不如把真实字段打出来看一眼。
func deviceInfoTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		ExternalID string `json:"external_id"`
		Name       string `json:"name"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &in)
	}
	ds, err := loadDeviceRegistry(ctx)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, d := range ds {
		if in.ExternalID != "" && d.ExternalID != in.ExternalID {
			continue
		}
		if in.Name != "" && d.Name != in.Name {
			continue
		}
		if strings.TrimSpace(d.Host) == "" {
			continue
		}
		tok, _, terr := ctx.VaultBlobOpenDevice("miio_token", d.ExternalID)
		if terr != nil || len(tok) != 16 {
			continue
		}
		env, ierr := miioExchangeWith(ctx, d, tok, "miIO.info", []any{},
			miioExchangeOptions{Attempts: 1})
		entry := map[string]any{"name": d.Name, "model": d.Model, "host": d.Host}
		if ierr != nil {
			entry["error"] = ierr.Error()
		} else {
			entry["info"] = env
		}
		out = append(out, entry)
		if in.ExternalID != "" || in.Name != "" {
			break
		}
	}
	return json.Marshal(map[string]any{"devices": out})
}

// fwVersionOf 从 miIO.info 响应里取固件版本。
//
// ⚠ 字段在 **result 里**，不在顶层（2026-11-17 实测台灯）：
//	{"id":..., "result":{"fw_ver":"1.3.9_0006","hw_ver":"ESP8266","model":"...","netif":{...}}}
// 我原来取顶层 `info["fw_ver"]` 所以全是空 —— 健康检查表里版本列一直显示 "-"。
//
// 不同型号字段名可能不同，按常见顺序退让；都取不到就返回空（前端显示 -）。
func fwVersionOf(info map[string]any) string {
	// 先把 result 层拉出来，再在原层找（有的固件确实放在顶层）
	scopes := []map[string]any{}
	if res, ok := info["result"].(map[string]any); ok {
		scopes = append(scopes, res)
	}
	scopes = append(scopes, info)
	for _, sc := range scopes {
		for _, k := range []string{"fw_ver", "fw_version", "application_version", "version"} {
			switch v := sc[k].(type) {
			case string:
				if strings.TrimSpace(v) != "" {
					return strings.TrimSpace(v)
				}
			case float64:
				return fmt.Sprintf("%.0f", v)
			}
		}
	}
	return ""
}
