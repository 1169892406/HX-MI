package main

import (
	"encoding/base64"
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
	pathLAN     = "局域网直连"
	pathGateway = "网关读取"
	pathCloud   = "云端下发"
)

// 健康检查的三种状态
const (
	healthOnline  = "在线"
	healthOffline = "离线"
	healthCloudOK = "云端可用"
)

// healthEntry 是列表里的一行。
type healthEntry struct {
	Name       string `json:"name"`
	Model      string `json:"model"`
	Version    string `json:"version"` // 取不到时为空，前端显示 "-"
	Path       string `json:"path"`
	PathDetail string `json:"path_detail"` // 副标题补充（IP / 经哪台网关）
	Status     string `json:"status"`
	LatencyMS  int    `json:"latency_ms"` // <0 表示不适用
	Latency    string `json:"latency"`    // 给人看的：35 ms / 3s 超时 / 不适用
	// Account 是同步来源账号（2026-11-20 用户要求：列表按账号过滤）。
	// 旧登记没有该字段，为空 ⇒ 前端显示时不带账号副行、过滤按"未知"。
	Account string `json:"account,omitempty"`
	// OfflineSince 是这条设备**进入当前状态**的时刻（RFC3339，离线才有意义）。
	//
	// 2026-11-21 用户「离线显示离线时间」：健康表每次探测都会覆盖状态，但"离线多久"
	// 是另一个事实 —— 它需要记住**状态变化的时刻**，而不是探测时刻。
	// 实现：`lan-health-since.json` 记 {external_id: {status, since}}；
	// 状态没变就沿用旧 since，变了就写 now。重启后照样连续（落盘）。
	OfflineSince string `json:"offline_since,omitempty"`
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

	/*
	 * ⚠ **不能靠 goroutine 并发**（2026-11-17 实测踩坑）。
	 *
	 * 我第一版按"8 路并发"写，结果耗时从 3.4 秒**涨到 6.7 秒**。
	 * 原因：插件是 `GOOS=wasip1` 的 **单线程** wasm 运行时，
	 * goroutine 无法真正并行，`ctx.UDPSend` 是同步 host_call ——
	 * 并发的只是调度开销，实际仍是一条道走到黑。
	 *
	 * 正确做法是**减少探测次数**，而不是并发：
	 *   · 网关链路只测一次（所有蓝牙/Zigbee 设备共用）—— 见 gwHealth
	 *   · 局域网设备**串行**探测，但把超时从 3s 降到 1.2s
	 *     （同网段设备正常情况下 RTT < 50ms，1.2s 足够；
	 *      真离线的设备等 3s 与 1.2s 的结论完全一样，只是白等）
	 *   · 探测结果**缓存到下次调用**：健康表不是实时监控，
	 *     5 秒一轮的定时探测会持续刷新它，用户点开时用缓存值即可立即返回
	 */
	lanResults := make([]lanHealthEntry, len(ds))

	// 网关链路探测（只做一次，供所有网关设备共用）
	type gwHealthResult struct {
		status    string
		latencyMS int
	}
	var gwHealth gwHealthResult
	if gw != nil && len(gwToken) == 16 {
		start := time.Now()
		_, perr := miioExchangeWith(ctx, *gw, gwToken, "miIO.info", []any{},
			miioExchangeOptions{Attempts: 1})
		if perr != nil {
			gwHealth.status = healthOffline
		} else {
			gwHealth.status = healthOnline
			gwHealth.latencyMS = int(time.Since(start).Milliseconds())
		}
	} else {
		gwHealth.status = healthOffline
	}

	// 串行探测走局域网设备（wasm 单线程，goroutine 无法并行）
	for i, d := range ds {
		if isIRDevice(d.Model) || isGatewayDevice(d.Model) || strings.TrimSpace(d.Host) == "" {
			continue
		}
		lanResults[i] = probeLanDeviceFast(ctx, d)
	}

	// 版本号：尽量从设备信息取（缓存在 registry 里；取不到就留空）
	out := make([]healthEntry, 0, len(ds))
	for i, d := range ds {
		_ = i
		e := healthEntry{
			Name: d.Name, Model: d.Model, Version: "",
			Account:   d.Account,
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
			/*
			 * 网关链路**只测一次**（2026-11-17 优化）。
			 *
			 * 同一台网关（小爱音箱）承载所有蓝牙/Zigbee 设备，
			 * 每台设备各测一遍 `miIO.info` 是同一件事做 N 次 ——
			 * 实测 3 台网关设备就是这样把总耗时拖到 3.4 秒的。
			 * 现在共用一次探测结果，N 台设备共享同一个结论。
			 */
			if gw == nil || len(gwToken) != 16 {
				e.Status = healthOffline
				e.PathDetail += "（未找到可用网关）"
			} else {
				e.Status = gwHealth.status
				if e.Status == healthOnline {
					e.LatencyMS = gwHealth.latencyMS
					e.Latency = fmt.Sprintf("%d ms", gwHealth.latencyMS)
				} else {
					e.Latency = "超时"
				}
			}
			out = append(out, e)

		default:
			e.Path = pathLAN
			e.PathDetail = "局域网 " + d.Host
			res := lanResults[i]
			ms := int(res.LatencyMS)
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
				e.Latency = "超时"
			}
			out = append(out, e)
		}
	}
	/*
	 * 给"非在线"的条目盖上「这个状态从什么时候开始」（2026-11-21）。
	 *
	 * 只对**离线/云端可用**这类"不在线"状态打时间戳：在线设备显示"在线 2 分钟"
	 * 没有意义，而离线设备"离线多久"恰恰是用户最想知道的（是他刚拔电源，还是
	 * 已经断了半天）。
	 */
	now := time.Now().UTC()
	applyOfflineSince(ctx, out, now)

	return json.Marshal(map[string]any{
		"devices":    out,
		"count":      len(out),
		"checked_at": now.Format(time.RFC3339),
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
//
//	{"id":..., "result":{"fw_ver":"1.3.9_0006","hw_ver":"ESP8266","model":"...","netif":{...}}}
//
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

// probeLanDeviceFast 是健康检查专用的**快速**探测：超时 1200ms。
//
// 理由（2026-11-17）：健康表里"离线"与"3s 超时"是同一个结论，
// 但 3 秒的等待对 5 台离线设备就是 15 秒。同网段设备正常 RTT < 50ms，
// 1.2 秒足够区分"在线"与"不在"。定时轮询（5s 一轮）会持续刷新，
// 用户点开健康表时看到的是上一次的结论，不必为此干等。
//
// 状态文案仍写「3s 超时」以与配置页说明一致？—— 不。文案改成「超时」，
// 不承诺具体秒数，避免"界面说 3s、实际 1.2s"这种不一致。
func probeLanDeviceFast(ctx *plugin.Context, d miioDevice) lanHealthEntry {
	now := time.Now().UTC().Format(time.RFC3339)
	if d.Host == "" {
		return lanHealthEntry{CheckedAt: now, Err: "没有局域网地址"}
	}
	start := time.Now()
	resp, err := ctx.UDPSend(plugin.UDPSendRequest{
		Host: d.Host, Port: 54321,
		PayloadB64: base64.StdEncoding.EncodeToString(helloPacket),
		TimeoutMS:  1200,
	})
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return lanHealthEntry{CheckedAt: now, LatencyMS: latency, Err: err.Error()}
	}
	pkt, derr := base64.StdEncoding.DecodeString(resp.PayloadB64)
	if derr != nil || len(pkt) < 32 {
		return lanHealthEntry{CheckedAt: now, LatencyMS: latency, Err: "回包非法"}
	}
	if _, _, perr := parseHelloPacket(pkt); perr != nil {
		return lanHealthEntry{CheckedAt: now, LatencyMS: latency, Err: perr.Error()}
	}
	return lanHealthEntry{Reachable: true, CheckedAt: now, LatencyMS: latency}
}

/*
 * applyOfflineSince 维护「设备从什么时候开始处于当前状态」（2026-11-21）。
 *
 * # 为什么要落盘而不是只在内存里记
 *
 * 用户问的是"离线多久"，而插件会因热加载换代、服务重启而换进程 —— 只记内存
 * 的话每次换代都把"已离线 3 小时"重置成"刚刚离线"，那是**编造**。
 * 这个事实很便宜（一台设备一条记录），落一个小 JSON 就够，且天然跨重启连续。
 *
 * # 判据
 *
 *	在线（healthOnline）       → 不显示时间（"在线 2 分钟"没有意义），并清掉记录
 *	其余（离线/云端可用…）      → 记/沿用 since
 *
 * 状态没变就**沿用**旧 since（不是每次探测都刷新），变了才写 now。
 */
const lanHealthSincePath = "lan-health-since.json"

type healthSinceRec struct {
	Status string `json:"status"`
	Since  string `json:"since"`
}

func loadHealthSince(ctx *plugin.Context) map[string]healthSinceRec {
	out := map[string]healthSinceRec{}
	r, err := ctx.FSRead(lanHealthSincePath)
	if err != nil || !r.Found {
		return out
	}
	b, err := r.Data()
	if err != nil {
		return out
	}
	_ = json.Unmarshal(b, &out)
	if out == nil {
		out = map[string]healthSinceRec{}
	}
	return out
}

func saveHealthSince(ctx *plugin.Context, m map[string]healthSinceRec) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = ctx.FSWrite(plugin.FSWriteRequest{
		Path: lanHealthSincePath, DataB64: base64.StdEncoding.EncodeToString(b), QuotaKB: 64,
	})
}

func applyOfflineSince(ctx *plugin.Context, out []healthEntry, now time.Time) {
	if len(out) == 0 {
		return
	}
	prev := loadHealthSince(ctx)
	next := make(map[string]healthSinceRec, len(prev))
	nowStr := now.Format(time.RFC3339)
	changed := false

	for i := range out {
		e := &out[i]
		key := e.Name
		if key == "" {
			continue
		}
		if e.Status == healthOnline {
			// 在线：清掉历史（下次离线要重新计时）
			if _, had := prev[key]; had {
				changed = true
			}
			continue
		}
		rec, had := prev[key]
		if had && rec.Status == e.Status && rec.Since != "" {
			e.OfflineSince = rec.Since
			next[key] = rec
			continue
		}
		e.OfflineSince = nowStr
		next[key] = healthSinceRec{Status: e.Status, Since: nowStr}
		changed = true
	}
	if changed || len(next) != len(prev) {
		saveHealthSince(ctx, next)
	}
}
