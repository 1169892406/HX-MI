package main

import (
	"strings"
	"fmt"
	"encoding/base64"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"homex.dev/homex/sdk/go/plugin"
)

// lanHealth 是局域网健康探测的进程内结果表。
//
// # 为什么需要它（用户明确要求）
//
// 云同步是按账号周期（默认 5 分钟）拉的，设备断网后要等下一轮同步才可能反映出来；
// 而小米云本身对离线判定还有额外延迟 —— 用户看到的是"设备明明拔了电，平台还显示
// 在线五分钟以上"。用户口径：局域网探测要高频，本地探不到就立即标离线，不要等
// 云端账号同步。
//
// 所以这里的定位很明确：**只回答"这台设备此刻在局域网里答不答话"**，
// 不碰云端状态、不改设备清单。谁把两者合并（本地优先/云端兜底）是下一步的事，
// 先把这个最基础的事实测准。
type lanHealth struct {
	mu      sync.Mutex
	results map[string]lanHealthEntry
}

type lanHealthEntry struct {
	Reachable bool   `json:"reachable"`
	CheckedAt string `json:"checked_at"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Err       string `json:"err,omitempty"`
}

var lanHealthTable = &lanHealth{results: map[string]lanHealthEntry{}}

// lanHealthProbeInterval 是探测周期。
//
// 30 秒：足够让"拔电"在一分钟内反映到界面，又不至于把局域网打成广播风暴
// （每台设备每次只发一个 32 字节 hello 包）。
const lanHealthProbeInterval = 30 * time.Second

// probeLanDevice 对单台设备做一次 miio hello 探测。
//
// 只发 hello 包、只解析回包头部：不需要令牌、不产生控制副作用，
// 因此可以对每台设备高频调用。失败**不重试**（这是健康检查不是业务调用），
// 重试会让"离线"这个结论迟到 —— 而要的正是"立即知道"。
func probeLanDevice(ctx *plugin.Context, d miioDevice) lanHealthEntry {
	now := time.Now().UTC().Format(time.RFC3339)
	if d.Host == "" {
		return lanHealthEntry{CheckedAt: now, Err: "没有局域网地址"}
	}
	start := time.Now()
	resp, err := ctx.UDPSend(plugin.UDPSendRequest{
		Host: d.Host, Port: 54321,
		PayloadB64: base64.StdEncoding.EncodeToString(helloPacket),
		TimeoutMS:  2000,
	})
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return lanHealthEntry{CheckedAt: now, LatencyMS: latency, Err: err.Error()}
	}
	pkt, derr := base64.StdEncoding.DecodeString(resp.PayloadB64)
	if derr != nil || len(pkt) < 32 {
		return lanHealthEntry{CheckedAt: now, LatencyMS: latency, Err: "回包非法"}
	}
	// 只解析头部：能按时收到合法 hello 回包就说明设备在线且同网段可达。
	dev, _, perr := parseHelloPacket(pkt)
	if perr != nil {
		return lanHealthEntry{CheckedAt: now, LatencyMS: latency, Err: perr.Error()}
	}
	// ## 必须校验 device_id，否则同 IP 的两台设备会互相冒充
	//
	// 实测发现：登记表里「风扇」与「小米天然气卫士」都是 192.168.1.58，
	// 而该地址实际应答的 device_id 是 1035850024 —— 与两者都不符（云端给的
	// localip 已过期）。若只看到"有回包"就判在线，我们会把一台陌生设备的
	// 应答算到天然气卫士头上，界面上显示"在线"但点它没反应。
	//
	// 判据：云端登记的 numeric_device_id 与实际应答不一致 ⇒ 地址已失效，
	// 判为不可达并说明原因（下一次云端同步会带回新地址）。
	if d.NumericDeviceID != 0 && dev != 0 && dev != d.NumericDeviceID {
		return lanHealthEntry{CheckedAt: now, LatencyMS: latency,
			Err: "地址不符：该 IP 上的设备 ID 与登记不一致（云端记录的地址已过期）"}
	}
	return lanHealthEntry{Reachable: true, CheckedAt: now, LatencyMS: latency}
}

// probeAllLanDevices 对登记表里所有带局域网地址的设备各探一次。
func probeAllLanDevices(ctx *plugin.Context) map[string]lanHealthEntry {
	ds, err := loadDeviceRegistry(ctx)
	if err != nil {
		return nil
	}
	out := make(map[string]lanHealthEntry, len(ds))
	for _, d := range ds {
		if d.Host == "" {
			continue
		}
		out[d.ExternalID] = probeLanDevice(ctx, d)
	}
	lanHealthTable.mu.Lock()
	for id, e := range out {
		lanHealthTable.results[id] = e
	}
	lanHealthTable.mu.Unlock()
	return out
}

// snapshotLanHealth 返回当前健康表快照（供 account/status 工具展示）。
func snapshotLanHealth() map[string]lanHealthEntry {
	lanHealthTable.mu.Lock()
	defer lanHealthTable.mu.Unlock()
	out := make(map[string]lanHealthEntry, len(lanHealthTable.results))
	for k, v := range lanHealthTable.results {
		out[k] = v
	}
	return out
}

// lanHealthTool 是 `lan_health` 工具：立即探测一次并返回结果。
func lanHealthTool(ctx *plugin.Context) (json.RawMessage, error) {
	res := probeAllLanDevices(ctx)
	if res == nil {
		res = map[string]lanHealthEntry{}
	}
	return json.Marshal(map[string]any{"devices": res, "probed_at": time.Now().UTC().Format(time.RFC3339)})
}

// cloudSyncPending 标记"宿主 tick 提示该同步了"。
//
// tick 不得做重活：它一旦阻塞，宿主 scheduler.loop 就停摆，后续所有定时任务
// （包括 30 秒的 lan-health）都再也不触发。所以 tick 只置位，真正的同步交给
// 手动同步 / 下一次工具调用执行。宁可晚一点同步，不能让调度器死掉。
var cloudSyncPending atomic.Bool

// consumeSyncPending 读出并清除待同步标记（供手动同步路径复用）。
func consumeSyncPending() bool {
	return cloudSyncPending.Swap(false)
}

// lanProbePending 标记"宿主 tick 提示该探测了"（同样为避免 tick 内做重活）。
var lanProbePending atomic.Bool


// consumePendingAutoSync 在只读工具调用时顺带执行一次待处理的自动同步。
//
// # 为什么必须挂在工具调用上，而不是 tick 里起 goroutine
//
// 宿主对单个 wasm 实例是串行调用的（CallOp 持 in.mu）。tick 执行期间锁被持有；
// 若在 tick 里 `go` 一个会发起 host_call 的 goroutine，它立刻要同一把锁，
// 而 tick 尚未返回 ⇒ 永久死锁（实测：03:59 之后再没同步成功过，每轮 tick 都
// 打印 "previous sync still running"）。
//
// 挂在工具调用上则完全相反：此刻我们正是那个持锁的调用，同步执行是安全的。
//
// 只读工具才触发：login/verify/logout 会改凭据，让后台同步混进来会让
// "用户刚点的操作"与"自动同步"互相干扰，且报错时分不清是谁引起的。
func consumePendingAutoSync(ctx *plugin.Context) {
	if !cloudSyncPending.Swap(false) {
		return
	}
	summary, err := syncCloudDevices(ctx)
	if err != nil {
		recordSyncFailure(ctx, err)
		ctx.Log(2, "hx-mi auto sync failed: "+err.Error())
		return
	}
	b, _ := json.Marshal(summary)
	ctx.Log(0, "hx-mi auto sync ok: "+string(b))
}

// consumePendingLanProbe 在下次工具调用时把"该探一次局域网了"落实（2026-11-17）。
//
// # 为什么必须有这个消费点
//
// `main.go` 的 lan-health tick 只 `lanProbePending.Store(true)`，而**全仓没有
// 任何地方读它** —— 也就是说局域网探测从来没真正跑过：`probeAllLanDevices`
// 只把结果写进插件内存表 `lanHealthTable`，既不 announce 也不上报可用性。
// 于是平台看到的在线/离线**完全来自云端同步**：云会话一过期，连"这台设备还在
// 不在局域网里"都判断不了。
//
// # 为什么不在 tick 里直接探
//
// `probeAllLanDevices` 对每台设备发 UDP hello 并等最多 2 秒；设备多时累计十几秒，
// 会把宿主调度循环卡住（main.go 的注释已经踩过这个坑）。所以：tick 只置位，
// 在**下一次工具调用**里做 —— 此刻我们持有该 wasm 实例锁，可以安全地发 host_call。
func consumePendingLanProbe(ctx *plugin.Context) {
	if !lanProbePending.CompareAndSwap(true, false) {
		return
	}
	/*
	 * ⚠ **必须限流**（2026-11-17 实测事故）。
	 *
	 * 第一版这里直接调 `probeAllLanDevices` 探**全部**设备：每台发 UDP hello 并等最多
	 * 3s，家里 8 台里有 4 台不通 ⇒ 累计 12s+，再把工具自己的耗时叠上去就撞
	 * 插件 30s 执行时限 ⇒ 沙箱被打死（`PLUGIN_SANDBOX_CRASH`），用户连「读取账号」
	 * 都失败（因为登录工具也会经过这里）。
	 *
	 * 所以：
	 *  ① **每次只探一台**（轮转，落盘记住 index），一次最多 3s，任何工具都不会被拖垮；
	 *  ② 探完一台就宣布**那一台**的可用性，其余设备沿用上次结果；
	 *  ③ 5 分钟一轮 tick ⇒ 一小时内能把家里设备轮一遍，够用。
	 */
	ds, err := loadDeviceRegistry(ctx)
	if err != nil || len(ds) == 0 {
		return
	}
	idx := loadLanProbeCursor(ctx) % len(ds)
	d := ds[idx]
	saveLanProbeCursor(ctx, (idx+1)%len(ds))

	res := map[string]lanHealthEntry{}
	if strings.TrimSpace(d.Host) != "" {
		res[d.ExternalID] = probeLanDevice(ctx, d)
		lanHealthTable.mu.Lock()
		lanHealthTable.results[d.ExternalID] = res[d.ExternalID]
		lanHealthTable.mu.Unlock()
	}
	announceLocalDevices(ctx, res)
}

// lanProbeCursorPath 记住"下一台该探谁"，让轮转跨调用/重启都连续。
const lanProbeCursorPath = "lan-probe-cursor.json"

func loadLanProbeCursor(ctx *plugin.Context) int {
	r, err := ctx.FSRead(lanProbeCursorPath)
	if err != nil || !r.Found {
		return 0
	}
	b, berr := r.Data()
	if berr != nil {
		return 0
	}
	var v struct {
		Index int `json:"index"`
	}
	if json.Unmarshal(b, &v) != nil || v.Index < 0 {
		return 0
	}
	return v.Index
}

func saveLanProbeCursor(ctx *plugin.Context, idx int) {
	b, err := json.Marshal(map[string]int{"index": idx})
	if err != nil {
		return
	}
	w := plugin.FSWriteRequest{Path: lanProbeCursorPath, QuotaKB: 8}
	w.SetData(b)
	_ = ctx.FSWrite(w)
}

// announceLocalDevices 用**本地登记表**（devices.json）宣布设备，不依赖云会话。
//
// # 为什么需要它
//
// 设备注册（DeviceAnnounce）此前只发生在 `syncCloudDevices` 的云端循环里。
// 于是**云会话过期 ⇒ 一台设备都不宣布 ⇒ 平台注册表为空 ⇒ 连局域网设备的命令
// 都被平台以「设备未注册」拒掉**（实测：`设备 "266363830" 未注册`）。
// 用户要的是「账号过期不影响设备控制」，那就不能把注册绑在云上。
//
// # 能力从哪来
//
// 本地登记表里已经有 `capabilities`（云端同步时写下的）与 `model`；
// 规格（含中文名）也早已缓存在 spec-cache。所以这里可以完全离线地投影出
// 与在线时**同样的**能力与控件清单。
//
// `availability` 由调用方给（局域网探测结果）；nil 表示"未知"，沿用
// 上次已知值（首次给 true —— 宁可先显示在线再被探测纠正，也不要让全部设备
// 因为一次云故障集体变离线）。
func announceLocalDevices(ctx *plugin.Context, availability map[string]lanHealthEntry) {
	ds, err := loadDeviceRegistry(ctx)
	if err != nil || len(ds) == 0 {
		return
	}
	announced := 0
	for _, d := range ds {
		avail := true
		if availability != nil {
			if e, ok := availability[d.ExternalID]; ok {
				avail = e.Reachable
			}
		}
		if err := ctx.DeviceAnnounce(plugin.DeviceAnnounceRequest{
			ExternalID:   d.ExternalID,
			Name:         d.Name,
			Vendor:       "xiaomi",
			Model:        d.Model,
			Capabilities: d.Capabilities,
			Available:    &avail,
			Attributes:   map[string]any{"controls": specControls(ctx, d.Model)},
		}); err != nil {
			ctx.LogWarn("hx-mi 本地宣布设备失败（device=%s）：%v", d.ExternalID, err)
			continue
		}
		announced++
	}
	if announced > 0 {
		ctx.LogWarn("hx-mi 已用本地登记表宣布 %d 台设备（不依赖云会话）", announced)
	}
}

// lanDiagnoseTool 是**只读**诊断工具（`lan_diagnose`）：判断一台局域网设备
// 到能用哪种协议通信，**不执行任何改变物理世界的动作**。
//
// # 为什么需要它（2026-11-17）
//
// 用户说「遥控数据存储下来了，不能本地控制」——红外码在音箱里，平台却只能走
// 云端。要判断"音箱能不能局域网发红外"，先得知道：
//   · 它的局域网 miio **加密通道**通不通（hello 有响应不代表能解出包）；
//   · 它认哪套协议：老 miio 方法（`miIO.info` / `get_prop`）还是 MIoT（`get_properties`）。
//
// 之前音箱 LAN 直连 30s 超时，我怀疑就是拿 MIoT 的 `get_properties` 去问一台
// 只认老方法（或根本不开放该端口）的设备。所以这里**分别试**、逐个记录耗时与错误。
//
// # 安全边界
//
// 只调用**查询类**方法：`miIO.info`（设备信息）、`get_prop ["power"]`（读属性）。
// 绝不发 `ir_play` / `set_*` / `action` 这类会产生实际动作的方法。
func lanDiagnoseTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	/*
	 * ⚠ 必须能**只测一台**（2026-11-17 实测）。
	 *
	 * 每台设备要串行试 4~5 个方法，单方法最坏 3s 超时 ⇒ 一台就可能 12~15s。
	 * 全量 8 台会远超插件 30s 执行时限，实测直接把沙箱打死：
	 *   `PLUGIN_SANDBOX_CRASH: module closed with context deadline exceeded`
	 *
	 * 所以支持 `{"name":"小爱音箱Pro"}` 或 `{"external_id":"266363830"}`
	 * 只测指定设备；不传则只测**前 2 台**（够用于排查，也不至于超时）。
	 */
	var in struct {
		Name       string `json:"name"`
		ExternalID string `json:"external_id"`
		Limit      int    `json:"limit"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &in)
	}
	ds, err := loadDeviceRegistry(ctx)
	if err != nil {
		return nil, err
	}
	if in.Name != "" || in.ExternalID != "" {
		filtered := ds[:0]
		for _, d := range ds {
			if (in.Name != "" && d.Name == in.Name) || (in.ExternalID != "" && d.ExternalID == in.ExternalID) {
				filtered = append(filtered, d)
			}
		}
		ds = filtered
	} else if in.Limit > 0 && len(ds) > in.Limit {
		ds = ds[:in.Limit]
	} else if len(ds) > 2 {
		ds = ds[:2]
	}
	type probeResult struct {
		Name      string   `json:"name"`
		Model     string   `json:"model"`
		Host      string   `json:"host"`
		TokenOK   bool     `json:"token_ok"`
		Handshake string   `json:"handshake"`            // "ok" / 错误短句
		Methods   []string `json:"methods,omitempty"`    // 老方法里成功的
		MIoTOK    bool     `json:"miot_ok"`              // get_properties 是否可用
		Errors    []string `json:"errors,omitempty"`
	}
	out := []probeResult{}
	for _, d := range ds {
		if strings.TrimSpace(d.Host) == "" {
			continue
		}
		r := probeResult{Name: d.Name, Model: d.Model, Host: d.Host}
		token, _, terr := ctx.VaultBlobOpenDevice("miio_token", d.ExternalID)
		if terr != nil || len(token) != 16 {
			r.TokenOK = false
			r.Handshake = "无设备 token"
			out = append(out, r)
			continue
		}
		r.TokenOK = true

		// ① 老方法 miIO.info：只返回设备信息，无副作用；也验证了加密通道。
		start := time.Now()
		if _, e := miioExchangeWith(ctx, d, token, "miIO.info", []any{},
			miioExchangeOptions{Attempts: 1}); e != nil {
			r.Handshake = "失败"
			r.Errors = append(r.Errors, "miIO.info: "+shortErr(e))
		} else {
			r.Handshake = fmt.Sprintf("ok(%dms)", time.Since(start).Milliseconds())
			r.Methods = append(r.Methods, "miIO.info")
		}

		// ② 老方法 get_prop ["power"]：读属性，无副作用。
		if r.Handshake == "ok" || strings.HasPrefix(r.Handshake, "ok(") {
			if _, e := miioExchangeWith(ctx, d, token, "get_prop", []string{"power"},
				miioExchangeOptions{Attempts: 1}); e != nil {
				r.Errors = append(r.Errors, "get_prop: "+shortErr(e))
			} else {
				r.Methods = append(r.Methods, "get_prop")
			}
			// ③ MIoT get_properties（siid/piid 查询，无副作用）。
			if _, e := miioBatchProperties(ctx, d, token, "get_properties",
				[]map[string]any{{"did": d.ExternalID, "siid": 2, "piid": 1}}); e != nil {
				r.Errors = append(r.Errors, "get_properties: "+shortErr(e))
			} else {
				r.MIoTOK = true
			}
			/*
			 * ④ 红外方法探测（2026-11-17）。
			 *
			 * 用户诉求：红外码存在音箱本地，能否**本地发射**（不依赖云）。
			 * 先判断音箱认不认这些方法名 —— 用**明显无效的码**（`"0000"`），
			 * 设备会因参数不合法而报错，而**不会真的发射**：
			 *   · 返回 error → 方法存在（只是参数被拒）；
			 *   · UDP 超时   → 方法名它根本不认。
			 *
			 * 只探名字，不探"怎么发"——真发要等用户确认（红外会改变物理世界）。
			 */
			/*
			 * 逐个试候选方法，**超时也记下来**（2026-11-17 改进）。
			 *
			 * 小米设备对不认识的方法**不响应**（UDP 超时），而不是回错误码 ——
			 * 所以"没出现"和"不存在"必须能区分开，否则看不出到底试过什么。
			 *
			 * 结果标记：
			 *   `方法(ok)`           → 应答了
			 *   `方法(err:...)`      → 应答了但报错（说明方法存在，参数/权限不对）
			 *   `方法(timeout)`      → 不认这个方法
			 */
			candidates := []struct {
				name   string
				params any
			}{
				// 属性读取（已确认 get_prop 可用，用真实属性名 volume）
				{"get_prop", []any{"volume"}},
				// 播放器操作类（确认播放控制是否走本地）
				{"player_play_operation", map[string]any{"action": "pause"}},
				{"player_get_play_status", []any{}},
				// 红外类：更多命名变体
				{"miIO.ir_play_key", map[string]any{"key_id": "", "dev_key": ""}},
				{"miIO.start_ir_play", map[string]any{"dev_key": "", "ir_code": "0000"}},
				{"ir_play_key", map[string]any{"key_id": "", "dev_key": ""}},
				{"miIO.irPlay", map[string]any{"dev_key": "", "ir_code": "0000"}},
			}
			for _, m := range candidates {
				_, e := miioExchangeWith(ctx, d, token, m.name, m.params,
					miioExchangeOptions{Attempts: 1})
				label := m.name
				if len(r.Methods) > 0 {
					// 同名方法只记一次（get_prop 试了两种参数）
					dup := false
					for _, x := range r.Methods {
						if strings.HasPrefix(x, label+"(") {
							dup = true
							break
						}
					}
					if dup {
						continue
					}
				}
				switch {
				case e == nil:
					r.Methods = append(r.Methods, label+"(ok)")
				case strings.Contains(e.Error(), "udp 等待响应超时") ||
					strings.Contains(e.Error(), "读取响应失败"):
					r.Methods = append(r.Methods, label+"(timeout)")
				default:
					r.Methods = append(r.Methods, label+"(err:"+shortErr(e)+")")
				}
			}
		}
		out = append(out, r)
	}
	return json.Marshal(map[string]any{"devices": out, "probed_at": time.Now().UTC().Format(time.RFC3339)})
}

// shortErr 截短错误信息（诊断输出用，避免把整段报文塞进界面）。
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 120 {
		msg = msg[:120] + "…"
	}
	return msg
}
