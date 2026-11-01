package main

import (
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
