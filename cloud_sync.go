package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"homex.dev/homex/sdk/go/plugin"
)

// cloudSyncMu 串行化同步：后台 tick 与手动 tool 不并发写 registry。
var cloudSyncMu sync.Mutex

// cloudDeviceView 是云端 device_list 的安全视图（不含明文 token）。
type cloudDeviceView struct {
	DID     string
	Token   string
	LocalIP string
	Name    string
	Model   string
	Online  bool
}

// openCloudSession 打开一个绑定已密封票据的宿主 HTTP 会话，并取回 ssecurity。
//
// 票据由登录流程 HTTPCookiesSeal 写入宿主 CookieJar；这里按同样三个 host
// Attach，随后所有 api.io.mi.com 请求自动带 Cookie。调用方负责 HTTPClose。
func openCloudSession(ctx *plugin.Context) (string, string, string, error) {
	r, err := ctx.FSRead("cloud-session.json")
	if err != nil || !r.Found {
		return "", "", "", errors.New("无云会话（需先登录）")
	}
	b, err := r.Data()
	if err != nil {
		return "", "", "", err
	}
	var saved savedCloudSession
	if json.Unmarshal(b, &saved) != nil || !saved.TicketsSealed || saved.SecretRef == "" {
		return "", "", "", errors.New("云会话票据不完整")
	}
	secret, _, err := ctx.VaultBlobOpen(saved.SecretRef)
	if err != nil {
		return "", "", "", fmt.Errorf("读取会话密钥失败: %w", err)
	}
	var material cloudSecretMaterial
	if json.Unmarshal(secret, &material) != nil || material.Ssecurity == "" {
		return "", "", "", errors.New("会话密钥材料损坏")
	}
	hs, err := ctx.HTTPOpen(plugin.HTTPOpenRequest{})
	if err != nil {
		return "", "", "", err
	}
	attached := 0
	for _, host := range []string{"account.xiaomi.com", "sts.api.io.mi.com", "api.io.mi.com"} {
		a, e := ctx.HTTPCookiesAttach(plugin.HTTPCookiesRequest{SessionID: hs.SessionID, Host: host})
		if e == nil {
			attached += len(a.Attached)
		}
	}
	if attached == 0 {
		_ = ctx.HTTPClose(hs.SessionID)
		return "", "", "", errors.New("没有可用的已密封票据")
	}
	return hs.SessionID, material.Ssecurity, saved.DeviceID, nil
}

// fetchCloudDevices 拉取云端设备清单（含 token，调用方不得记录/回显）。
func fetchCloudDevices(ctx *plugin.Context, sessionID, ssecurity, deviceID string) ([]cloudDeviceView, error) {
	const data = `{"getVirtualModel":true,"getHuamiDevices":1,"get_split_device":false,"support_smart_home":true}`
	body, err := apiPostRC4GuestDevice(ctx, sessionID, ssecurity, deviceID, "https://api.io.mi.com/app/home/device_list", data)
	if err != nil {
		return nil, err
	}
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(body, &env) != nil || len(env.Result) == 0 {
		return nil, errors.New("设备清单响应不确定")
	}
	var result struct {
		List []map[string]any `json:"list"`
	}
	if json.Unmarshal(env.Result, &result) != nil {
		return nil, errors.New("设备清单结构不确定")
	}
	out := make([]cloudDeviceView, 0, len(result.List))
	for _, m := range result.List {
		did := strings.TrimSpace(fmt.Sprint(m["did"]))
		if did == "" || did == "<nil>" {
			continue
		}
		out = append(out, cloudDeviceView{
			DID:     did,
			Token:   strings.TrimSpace(fmt.Sprint(m["token"])),
			LocalIP: strings.TrimSpace(fmt.Sprint(m["localip"])),
			Name:    strings.TrimSpace(fmt.Sprint(m["name"])),
			Model:   strings.TrimSpace(fmt.Sprint(m["model"])),
			Online:  m["isOnline"] == true || fmt.Sprint(m["isOnline"]) == "1",
		})
	}
	return out, nil
}

// syncCloudDevices 把云端清单合并进 registry v3，并密封 token、公告设备。
//
// 身份规则（已按生产平台核实）：external_id = 小米 DID。平台现有 devices
// 行的 external_id 就是 DID（如 237970870），因此按 DID 合并不会产生重复设备。
func syncCloudDevices(ctx *plugin.Context) (map[string]int, error) {
	cloudSyncMu.Lock()
	defer cloudSyncMu.Unlock()
	// running 是进程内瞬时事实：配置页要能区分"同步中"与"上次同步在 X 点"。
	syncInFlight.Store(true)
	defer syncInFlight.Store(false)
	summary := map[string]int{"cloud": 0, "added": 0, "updated": 0, "announced": 0, "tokens": 0, "failed": 0}

	/*
	 * 同步的墙钟预算（2026-11-21）。
	 *
	 * 宿主按 installation 的 execution_timeout_ms（30s）杀模块，插件必须自己
	 * 先守住这个预算 —— 否则"某一轮慢"升级成"实例被判死"（用户看到
	 * 「读取账号失败：沙箱实例已不可用」）。这里取 20s 作为软上限：留 10s 给
	 * 返回值序列化与宿主侧开销，任何"读状态"的循环到了预算就停手，
	 * 本轮先把已读到的结果落盘/上报，剩下的下一轮继续。
	 */
	syncDeadline := time.Now().Add(20 * time.Second)
	overBudget := func() bool { return time.Now().After(syncDeadline) }

	sessionID, ssecurity, deviceID, err := openCloudSession(ctx)
	if err != nil {
		recordSyncFailure(ctx, err)
		return summary, err
	}
	defer func() { _ = ctx.HTTPClose(sessionID) }()

	// 会话账号（2026-11-20 用户要求：局域网检查列表按账号过滤）。
	// 本轮同步拉到的每台设备都盖这个账号戳 —— 换账号同步后，旧账号的设备
	// 保留旧戳，过滤因此能区分"这些设备是谁带进来的"。
	sessionAccount := ""
	if id := loadAccountIdentity(ctx); id != nil {
		sessionAccount = id.Account
	}

	cloud, err := fetchCloudDevices(ctx, sessionID, ssecurity, deviceID)
	if err != nil {
		recordSyncFailure(ctx, err)
		return summary, err
	}
	summary["cloud"] = len(cloud)

	ds, err := loadDeviceRegistry(ctx)
	if err != nil && !strings.Contains(err.Error(), "设备登记不存在") {
		return summary, err
	}
	index := map[string]int{}
	for i := range ds {
		index[ds[i].ExternalID] = i
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_ = now

	for _, c := range cloud {
		external := c.DID
		if external == "" {
			continue
		}
		if !modelAllowed(c.Model) {
			continue
		}
		/*
		 * ## capability 必须有保底（2026-11-10 根修「设备进不了待绑定列表」）
		 *
		 * 用户实测：云端 12 台、可本地控制 8 台都数对了，但平台侧设备数 **0**。
		 * 真因：`projectCapabilities` 依赖 MIoT 型号目录的 Binding 投影，
		 * 目录未构建/型号未收录时它返回 error，这里就当空 caps 继续，
		 * 于是 `device.announce` 全被平台拒：
		 *   `device.announce 必须声明至少一个 capability`
		 * 而那个错误只被计进 failed（连日志都没有）—— 界面上完全看不出来。
		 *
		 * 兜底顺序：① 目录投影；② 已登记设备里学到的（上一次成功投影过的）；
		 * ③ 型号前缀推断（plug/light/sensor/… 都是稳定的命名事实）。
		 * 三个都拿不到才用 `sensor`（读状态永远成立），并把原因记进日志 ——
		 * 宁可"能力少报一个"也不要"整台设备进不来"：设备先能被看见，能力可以之后修正。
		 */
		caps := []string{}
		if c.Model != "" {
			if projected, perr := projectCapabilities(ctx, c.Model); perr == nil && len(projected) > 0 {
				caps = projected
			}
		}
		if len(caps) == 0 {
			if i, ok := index[external]; ok && len(ds[i].Capabilities) > 0 {
				caps = append([]string(nil), ds[i].Capabilities...)
			}
		}
		if len(caps) == 0 && c.Model != "" {
			caps = fallbackCapabilities(c.Model)
		}
		if len(caps) == 0 {
			caps = []string{"sensor"}
		}
		/*
		 * ★★★ 2026-11-16（用户「正常这个台灯还有温度数据啊，现在怎么看不见」）：
		 *
		 * 兜底投影只按型号前缀猜（`plug` → 只有 `switch`），而 `chuangmi.plug.m3`
		 * 这类插座**规格里本来就有温度属性**（siid=2/piid=2，已用缓存 spec 核对）。
		 * 不补上 `sensor` 能力槽，平台就没有地方存温度 —— 插件读到了也显示不出来。
		 *
		 * 判据用**规格本身**（而不是再写一张型号表）：规格里出现 temperature 属性
		 * 就补 sensor。这样新型号不用改代码，只要目录能拉到规格就自动生效。
		 */
		if !stringInSlice(caps, "sensor") && modelHasTemperature(ctx, c.Model) {
			caps = append(caps, "sensor")
		}
		/*
		 * 红外设备（电视/空调遥控）必须报出**可控**能力（2026-11-17）。
		 *
		 * 这类设备没有局域网地址，兜底投影只按型号前缀猜（`ir`/`television`
		 * 都不在前缀表里 ⇒ 退到 `sensor`），于是平台把它当只读传感器 ——
		 * 遥控器明明能开关电视，界面上却一个按钮都没有。
		 *
		 * 判据用规格里**是否真的有 turn-on 动作**（不是型号表）：
		 * 电视 `miir-tv-ir01` 的 service 2 就有 turn-on/turn-off/volume/mute；
		 * 空调 `miir-aircondition-ir02` 同样有 turn-on/turn-off。
		 * 平板能力名用 `switch`（平台 UI 据此渲染开关，语义也准确：开/关）。
		 */
		if !stringInSlice(caps, "switch") && modelHasTurnOnAction(ctx, c.Model) {
			caps = append(caps, "switch")
		}
		/*
		 * 规格驱动的**可写属性与动作**（2026-11-17 用户截图那 10 项）。
		 *
		 * HA 那个插件按规格动态生成实体：每个可写属性一个开关、每个动作一个按钮，
		 * 所以同一台音箱能出「麦克风静音 / 睡眠模式 / 停止闹钟 / 播放电台 / 唤醒」
		 * 这些命名控件。我们平台按能力类型渲染固定控件，所以要把规格里的东西
		 * **先投影成能力实例**，界面才有位置挂。
		 *
		 * 具体做法见 projectSpecControls：给每个可写 bool 属性生成一个
		 * `switch` 实例（instance_id 用属性名），给每个可写数值属性生成一个
		 * `number` 实例，给每个动作生成一个 `button` 实例。
		 */
		extraCaps := projectSpecControls(ctx, c.Model)
		for _, ec := range extraCaps {
			if !stringInSlice(caps, ec) {
				caps = append(caps, ec)
			}
		}
		host := c.LocalIP
		name := c.Name
		if name == "" {
			name = c.Model
		}
		if i, ok := index[external]; ok {
			prev := ds[i]
			if host != "" {
				ds[i].Host = host
			}
			if sessionAccount != "" {
				ds[i].Account = sessionAccount
			}
			if c.Model != "" {
				ds[i].Model = c.Model
			}
			if name != "" {
				ds[i].Name = name
			}
			if len(caps) > 0 {
				ds[i].Capabilities = mergeCapabilities(prev.Capabilities, caps)
			}
			if nid, perr := strconv.ParseUint(external, 10, 32); perr == nil {
				ds[i].NumericDeviceID = uint32(nid)
			}
			summary["updated"]++
		} else {
			d := miioDevice{
				ExternalID: external, Host: host, TokenRef: "miio_token",
				Model: c.Model, Name: name, Capabilities: caps,
				Account: sessionAccount,
			}
			if nid, perr := strconv.ParseUint(external, 10, 32); perr == nil {
				d.NumericDeviceID = uint32(nid)
			}
			ds = append(ds, d)
			index[external] = len(ds) - 1
			summary["added"]++
		}

		// 先公告，再写设备级 Secret：宿主按 DriverRegistry 校验设备归属。
		avail := c.Online
		if err := ctx.DeviceAnnounce(plugin.DeviceAnnounceRequest{
			ExternalID: external, Name: name, Vendor: "xiaomi", Model: c.Model,
			Capabilities: ds[index[external]].Capabilities, Available: &avail,
			// 规格驱动的控件明细（2026-11-17）：平台据此渲染"麦克风静音/停止闹钟"
			// 这类**命名**控件，而不是一个笼统的"开关/按下"。
			Attributes: map[string]any{"controls": filterControlsForDevice(ctx, external, c.Model, specControlsCached(ctx, c.Model))},
		}); err != nil {
			/*
			 * ⚠ 必须记下**原因**（2026-11-10 用户实测"设备没进待绑定列表"）。
			 *
			 * 原来这里只 `failed++` 就 continue —— 界面上只看到"云端 12 台、
			 * announced 0"，却完全不知道平台为什么拒。这类"计数正确但原因丢失"
			 * 的写法把排查成本全推给了用户（他只能报"没进来啊"）。
			 */
			summary["failed"]++
			ctx.LogWarn("hx-mi device announce failed: external_id=%s model=%s caps=%v err=%v",
				external, c.Model, ds[index[external]].Capabilities, err)
			continue
		}
		summary["announced"]++

		/*
		 * BLE 设备的状态（2026-11-16 用户「温湿度计没有状态」）。
		 *
		 * 这类设备 `host` 为空 —— 没有 IP，本地 miio 完全无从下手，只能走云端。
		 * 两条约束：
		 *  1. **只读传感器类**（型号里含 "sensor"）：同步是 30s 预算内的任务，
		 *     对十几台 BLE 设备逐个发云端请求会把预算打满，反而拖垮同步本身；
		 *  2. 失败只 Warn 不中断：一台读不到不该让整轮同步失败。
		 */
		/*
		 * LAN 传感器周期读取（2026-11-17）。
		 *
		 * 之前状态上报**只在下发命令时**发生 —— 而传感器没有命令，于是
		 * `小米天然气卫士` 这类设备永远没有数据（用户报"传感器看不见"）。
		 * 同步每轮顺带读一次，是这类设备唯一可行的取数方式。
		 */
		/*
		 * 每轮**最多轮询一台**传感器（2026-11-17）。
		 *
		 * 实测：LAN 读取失败时 miio 会重试 3 次 × 3s = 9s，两三台就把同步的
		 * 30s 预算吃光（`sync_now` 直接超时）。传感器数据不需要"每轮全量"，
		 * 轮着来即可：一台/轮，5 分钟一轮，几分钟内全部覆盖一遍。
		 */
		/*
		 * 需要**周期轮询状态**的设备（每轮最多 1 台，见下）：
		 *   · 传感器（host 非空 + 型号含 sensor/gas/...）—— 没有命令可触发上报；
		 *   · 音响/播放器 —— 云端状态滞后，命令后回读拿到的是旧值，
		 *     只能靠"稍后再读"（2026-11-17 实测滞后一轮）。
		 */
		isMedia := hasCap(ds[index[external]].Capabilities, "media_player") ||
			strings.Contains(strings.ToLower(c.Model), "speaker")
		/*
		 * 每轮轮询**上限**（2026-11-17 用户报「设备状态都是未知」）。
		 *
		 * 原来是 `summary["states"] < 1` —— **每轮只读 1 台**。家里 11 台设备、
		 * 同步 5 分钟一轮 ⇒ 轮完一遍要 **55 分钟**，用户点开设备看到的自然是
		 * 十几小时前的旧值，甚至从来没有值（"未知"）。
		 *
		 * 改成 `statePollBudget`：按设备数自适应（见该常量注释），
		 * 局域网直连读很快（实测 3~35ms），云端读慢（百毫秒~秒级）。
		 */
		pollSensor := false
		if isSensorLikeModel(c.Model) || isMedia {
			if isMedia || strings.TrimSpace(c.LocalIP) != "" {
				limit := statePollBudget(len(ds))
				// 到预算就停止读状态：宁可这一轮少读几台，也不能撞宿主执行时限
				// （2026-11-21：撞上限 = 模块被杀 = 整轮同步白做 + 实例判死）。
				pollSensor = summary["states"] < limit && !overBudget()
			}
		}
		if pollSensor && isMedia {
			/*
			 * 本地优先（2026-11-17）：音箱支持 `get_prop ["volume","mute"]`，
			 * 这条不依赖云会话。云一过期，音量/静音至少还能看。
			 */
			var mst map[string]any
			var mserr error
			if tok, _, terr := ctx.VaultBlobOpenDevice("miio_token", external); terr == nil && len(tok) == 16 &&
				strings.TrimSpace(c.LocalIP) != "" {
				mst, mserr = lanMediaState(ctx, ds[index[external]], tok)
			} else {
				mserr = errors.New("无局域网 token/地址")
			}
			if mserr != nil {
				mst, mserr = cloudMediaState(ctx, ds[index[external]])
			}
			if st, serr := mst, mserr; serr != nil {
				ctx.LogWarn("hx-mi 媒体状态轮询失败（device=%s model=%s）：%v", external, c.Model, serr)
			} else if len(st) > 0 {
				if perr := ctx.DeviceStatePublish(plugin.DeviceStateRequest{
					ExternalID: external, State: st,
					ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
				}); perr != nil {
					ctx.LogWarn("hx-mi 媒体状态上报失败（device=%s）：%v", external, perr)
				} else {
					summary["states"]++
				}
			}
		}
		if pollSensor && !isMedia {
			/*
			 * 先本地、失败再云端（2026-11-17）。
			 *
			 * 实测 `lumi.sensor_gas.mcn02` 的 `localip` 指向的地址直连
			 * `udp 等待响应超时` —— 它是**经网关的 Zigbee 子设备**，本地直连不通。
			 * Wi-Fi 传感器则本地就够（快、不依赖外网）。
			 * 所以两条都试：本地优先，失败回落云端（云端会经网关转发）。
			 */
			var st map[string]any
			var serr error
			if strings.HasPrefix(strings.ToLower(c.Model), "lumi.") {
				// lumi.* 是**网关的 Zigbee 子设备**：localip 直连必然超时，
				// 直接走云端（云端经网关转发），省掉 9s 的无谓重试。
				st, serr = readSensorStateViaCloud(ctx, ds[index[external]], external,
					sessionID, ssecurity, deviceID)
			} else {
				st, serr = readLANSensorState(ctx, ds[index[external]], external)
				if serr != nil {
					if cst, cerr := readSensorStateViaCloud(ctx, ds[index[external]], external,
						sessionID, ssecurity, deviceID); cerr == nil {
						st, serr = cst, nil
					}
				}
			}
			if serr != nil {
				ctx.LogWarn("hx-mi 传感器读取失败（device=%s model=%s）：%v", external, c.Model, serr)
			}
			if len(st) > 0 {
				if perr := ctx.DeviceStatePublish(plugin.DeviceStateRequest{
					ExternalID: external, State: st,
					ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
				}); perr != nil {
					ctx.LogWarn("hx-mi LAN 传感器状态上报失败（device=%s）：%v", external, perr)
				} else {
					summary["states"]++
				}
			}
		}
		if strings.TrimSpace(c.LocalIP) == "" && strings.Contains(strings.ToLower(c.Model), "sensor") {
			if st, serr := cloudReadSensorState(ctx, sessionID, ssecurity, deviceID, external); serr != nil {
				ctx.LogWarn("hx-mi 云端读取传感器状态失败（device=%s model=%s）：%v", external, c.Model, serr)
			} else if perr := ctx.DeviceStatePublish(plugin.DeviceStateRequest{
				ExternalID: external, State: st,
				ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
			}); perr != nil {
				ctx.LogWarn("hx-mi 传感器状态上报失败（device=%s）：%v", external, perr)
			} else {
				summary["states"]++
			}
		}

		if c.Token != "" {
			tok, derr := hex.DecodeString(c.Token)
			if derr == nil && len(tok) == 16 {
				if _, serr := ctx.VaultBlobSealDevice("miio_token", external, tok); serr == nil {
					summary["tokens"]++
				} else {
					ctx.Log(2, "hx-mi sync seal token failed: "+serr.Error())
				}
			}
		}
	}

	// 空列表要写成 []，不能写成 null：null 会让"云端没有设备"看起来像坏文件
	//（loadDeviceRegistry 曾因此报「设备登记损坏」）。
	if ds == nil {
		ds = []miioDevice{}
	}
	payload, _ := json.Marshal(ds)
	sum := sha256Hex(payload)
	reg, _ := json.Marshal(deviceRegistryV2{Version: 3, Devices: ds, SHA256: sum})
	w := plugin.FSWriteRequest{Path: "devices.json", QuotaKB: 4096}
	w.SetData(reg)
	if err := ctx.FSWrite(w); err != nil {
		recordSyncFailure(ctx, err)
		return summary, fmt.Errorf("registry v3 写入失败: %w", err)
	}
	// 落盘这次同步的**结果快照**：配置页的设备表、三个计数、最近成功时间都来自它。
	// 必须在这里记（而不是在 account 工具里现算）：现算只能回答"现在能连上吗"，
	// 回答不了"上次成功同步到的是哪几台、几点成功的"。
	recordSyncSuccess(ctx, buildSyncDeviceViews(ctx, cloud, ds))
	return summary, nil
}

// buildSyncDeviceViews 把云端清单与本地 registry 合成配置页要的安全视图。
//
// 字段口径照旧版 httpapi.SyncDeviceView：
//   - controllable = 有令牌（TokenRef 已密封）且有局域网地址 → 平台上点得动
//   - available    = 云端清单里仍存在（本次同步到它了 ⇒ true）
//   - online       = 云端上报的实时在线状态（与 available 是两件事）
//
// 不含令牌、IP、MAC：配置页只是给人看的。
func buildSyncDeviceViews(ctx *plugin.Context, cloud []cloudDeviceView, ds []miioDevice) []syncDeviceView {
	byID := map[string]miioDevice{}
	for _, d := range ds {
		byID[d.ExternalID] = d
	}
	out := make([]syncDeviceView, 0, len(cloud))
	for _, c := range cloud {
		local, registered := byID[c.DID]
		view := syncDeviceView{
			DID: c.DID, Name: c.Name, Model: c.Model,
			Available: true, Online: c.Online,
		}
		if view.Name == "" {
			view.Name = local.Name
		}
		if view.Model == "" {
			view.Model = local.Model
		}
		// 账号戳（2026-11-20）：登记在同步流程里已盖过章，这里如实带出。
		if registered {
			view.Account = local.Account
		}
		hasToken := registered && local.TokenRef != ""
		hasAddress := registered && local.Host != ""
		view.Controllable = hasToken && hasAddress
		if !view.Controllable {
			switch {
			case !registered:
				view.Reason = "not_registered"
				view.ReasonText = "尚未登记到本机（下一次同步或需重启插件）"
			case !hasToken:
				view.Reason = "non_miio"
				view.ReasonText = "云端未下发令牌（可能是蓝牙设备或需网关）"
			default:
				view.Reason = "no_address"
				view.ReasonText = "没有局域网地址（需与设备同网段或开启云控）"
			}
		}
		out = append(out, view)
	}
	return out
}

// modelAllowed 按 device_filter_models 前缀过滤；空列表 = 全部允许。
/*
 * fallbackCapabilities 按型号前缀给出最小可用能力集。
 *
 * # 为什么要有它
 *
 * 平台的 device.announce 要求至少一个 capability，而 capability 来自 MIoT 目录
 * 的 Binding 投影 —— 目录没构建、型号没收录、投影失败时就是空。
 * 空 capability = **整台设备被拒**，用户看到的是"云端有 12 台、平台一台都没有"。
 *
 * 这里的映射只依据**型号命名里稳定的事实**（各厂商沿用小写点分前缀），
 * 不猜复杂能力：宁可少报（之后目录建好会覆盖成精确值），也不要漏设备。
 * 顺序有意义：先匹配更具体的（plug/light）再退到 sensor。
 */

// stringInSlice 小工具（本文件用得多，避免每处手写循环）。
func stringInSlice(list []string, target string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}

// modelHasTurnOnAction 判断型号的 MIoT 规格里是否有 turn-on 动作（红外遥控的判据）。
//
// 与 modelHasTemperature 同一思路：按**规格事实**判断，不维护型号表 ——
// 红外型号（miir-tv-ir01 / miir-aircondition-ir02 / xiaomi.ir.*）各不相同，
// 写表必漏，而规格里动作名是稳定的 urn（`:turn-on:`）。
func modelHasTurnOnAction(ctx *plugin.Context, model string) bool {
	if model == "" {
		return false
	}
	typ, ok := lookupModelType(model)
	if !ok {
		return false
	}
	sp, err := loadMIoTSpec(ctx, typ)
	if err != nil {
		return false
	}
	for _, svc := range sp.Services {
		for _, a := range svc.Actions {
			if strings.Contains(strings.ToLower(a.Type), ":turn-on:") {
				return true
			}
		}
	}
	return false
}

// modelHasTemperature 判断该型号的 MIoT 规格里是否有温度属性。
//
// # 为什么不写死型号表
//
// 有温度传感器的插座/开关型号很多（chuangmi.plug.m1/m3、各类温控开关…），
// 维护一张表必然漏。规格文件本身已经是权威 —— 目录能拉到就算数。
//
// # 取不到规格时返回 false
//
// 这是**增强**判据：拿不到规格就不要凭空加能力槽（否则又回到"能力乱报"）。
func modelHasTemperature(ctx *plugin.Context, model string) bool {
	if model == "" {
		return false
	}
	typ, ok := lookupModelType(model)
	if !ok {
		return false
	}
	sp, err := loadMIoTSpec(ctx, typ)
	if err != nil {
		return false
	}
	for _, svc := range sp.Services {
		for _, pr := range svc.Properties {
			if strings.Contains(strings.ToLower(pr.Type), "temperature") {
				return true
			}
		}
	}
	return false
}

func fallbackCapabilities(model string) []string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "plug"), strings.Contains(m, "switch"):
		return []string{"switch"}
	case strings.Contains(m, "light"), strings.Contains(m, "lamp"), strings.Contains(m, "ceiling"):
		return []string{"light"}
	case strings.Contains(m, "aircondition"), strings.Contains(m, "acpartner"),
		strings.Contains(m, "heater"), strings.Contains(m, "thermostat"):
		return []string{"climate"}
	case strings.Contains(m, "curtain"), strings.Contains(m, "cover"):
		return []string{"cover"}
	case strings.Contains(m, "sensor"), strings.Contains(m, "ht"), strings.Contains(m, "weather"):
		return []string{"sensor"}
	case strings.Contains(m, "fan"), strings.Contains(m, "purifier"), strings.Contains(m, "humidifier"):
		return []string{"switch"}
	case strings.Contains(m, "remote"), strings.Contains(m, "ir"):
		return []string{"sensor"}
	case strings.Contains(m, "speaker"), strings.Contains(m, "wifispeaker"):
		return []string{"sensor"}
	// 剩余的家电类（榨汁机/牙刷/锅/净化器/加湿器…）：它们都能被开关或读状态，
	// `switch` 是这里最保守且最有用的选择（能被"打开/关闭"至少说明它活着）。
	case strings.Contains(m, "juicer"), strings.Contains(m, "toothbrush"),
		strings.Contains(m, "cooker"), strings.Contains(m, "kettle"),
		strings.Contains(m, "vacuum"), strings.Contains(m, "printer"):
		return []string{"switch"}
	}
	// **兜底不返回 nil**：返回 nil 等于"这台设备一定被平台拒"。
	// 未知型号给 `sensor`（读状态永远成立）—— 设备先能被看见，
	// 能力之后由目录投影修正。见云同步里那段"宁可少报一个能力"的注释。
	return []string{"sensor"}
}

func modelAllowed(model string) bool {
	if len(current.DeviceFilterModels) == 0 {
		return true
	}
	for _, f := range current.DeviceFilterModels {
		if strings.HasPrefix(model, strings.TrimSpace(f)) {
			return true
		}
	}
	return false
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return fmt.Sprintf("%x", s[:])
}

// specControls 把规格里的可写属性与动作导出成平台能渲染的控件描述。
//
// 契约（平台 adapter 的 capsToSpecs 读它）：
//
//	[{"capability":"switch","instance_id":"mute","name":"麦克风静音","format":"bool","write":true},
//	 {"capability":"button","instance_id":"stop-alarm","name":"停止闹钟","action":"stop-alarm"},
//	 {"capability":"number","instance_id":"volume","name":"音量","format":"uint8","min":0,"max":100}]
//
// name 用**规格的中文名**（multiLanguage 字典，如「播放电台」）；取不到就用人话化
// 的英文名（`Play Radio`），绝不显示 `play_radio` 这种内部串。
//
// 只导 service≥2 的属性/动作：service 1 是厂商/型号/固件，不是控件。
func specControls(ctx *plugin.Context, model string) []map[string]any {
	typ, ok := lookupModelType(model)
	if !ok {
		return nil
	}
	sp, err := loadMIoTSpec(ctx, typ)
	if err != nil {
		return nil
	}
	return buildSpecControls(ctx, model, sp)
}

/*
 * specControlsCached 是 specControls 的**只读缓存**版本：规格不在本地缓存里就
 * 返回 nil，**绝不联网**。
 *
 * ## 为什么必须分开（2026-11-21 实测「沙箱实例已不可用（closed）」）
 *
 * 云同步是个 30 秒预算内的调用：一台设备一次 `loadMIoTSpec` 在缓存缺失时会去
 * miot-spec.org 拉规格（单次上限 15s），家里十几台新设备 ⇒ 一次同步轻松撞满
 * 30s ⇒ 宿主按 execution_timeout 杀掉模块 ⇒ 整轮同步失败、实例判死。
 * 日志里那条 `reason: module closed with context deadline exceeded`（每 5 分钟
 * 同步一轮出现一次）就是它。
 *
 * 同步的职责是"把设备登记进来"，不是"顺手把全世界规格都下完"：
 *   · 缓存里有 ⇒ 带上控件明细（平台立刻能渲染命名控件）；
 *   · 缓存里没有 ⇒ 这一轮先不带，等用户点开设备详情/设置时由
 *     `device_detail`（交互式调用，用户预期等待）去拉并落缓存。
 *
 * 这样同步的耗时与设备数线性但**没有网络长尾**，不会再撞执行预算。
 */
/*
 * filterControlsForDevice 对**老 miio 协议设备**裁掉规格里实际不通的属性。
 *
 * ★★★ 2026-11-24（用户「HX-MI 接入的设备控制开关坏的，配置也是坏的」）
 *
 * 实测事实（台灯 chuangmi.plug.m3，已被 miio-protocol.json 缓存为老协议）：
 *   · 云端 MIoT spec 说它有 service 3 指示灯（标 write）；
 *   · 但局域网 get_properties 对它稳定回 `code:-30001 Resp invalid json`；
 *   · 局域网 set_properties 3.1 "成功"却是固件把它当主开关（写 3.1=false 会把
 *     2.1 一起关掉，device_switch.go 注释里早有实测）。
 * 也就是说**规格与固件不一致**：spec 承诺的指示灯在这台固件上既读不到也控不了。
 *
 * 参考实现 hass-xiaomi-miot 对这台设备的映射是：
 *     'prop.2.1': power   → get_prop / set_power
 *     'prop.2.2': temperature
 * —— **没有 3.1**。老协议设备只有这些"字符串属性名"可控可读，MIoT 的 (siid,piid)
 * 在它身上不是有效接口。所以老协议设备只保留白名单里的属性，不把假能力投影给平台
 * （投了就是「点开关亮的却是另一个」「指示灯永远未知」这类用户看到的功能错乱）。
 *
 * 白名单按参考实现的 miio_specs 口径；没列出的 (siid,piid) 一律裁掉。
 * 非老协议设备原样返回（它们的 MIoT 接口是通的）。
 */
var legacyWritableProps = map[string]bool{
	"2.1": true, // power（主开关）
	"2.2": true, // temperature（只读，保留给传感器用）
}

func filterControlsForDevice(ctx *plugin.Context, externalID, model string, controls []map[string]any) []map[string]any {
	if len(controls) == 0 || externalID == "" {
		return controls
	}
	if !loadLegacyDevices(ctx)[externalID] {
		return controls
	}
	out := make([]map[string]any, 0, len(controls))
	for _, c := range controls {
		siid, _ := c["siid"].(int)
		piid, _ := c["piid"].(int)
		key := fmt.Sprintf("%d.%d", siid, piid)
		if legacyWritableProps[key] {
			out = append(out, c)
			continue
		}
		ctx.Log(1, fmt.Sprintf("hx-mi 老协议设备裁掉规格属性 %s（device=%s model=%s）：该固件不支持 MIoT (siid,piid) 读写", key, externalID, model))
	}
	return out
}

func specControlsCached(ctx *plugin.Context, model string) []map[string]any {
	if _, ok := lookupModelType(model); !ok {
		return nil
	}
	sp, ok := readCachedSpec(ctx, "spec-cache/"+safeModel(model)+".json", model)
	if !ok || len(sp.Services) == 0 {
		return nil
	}
	return buildSpecControls(ctx, model, sp)
}

func buildSpecControls(ctx *plugin.Context, model string, sp miotSpec) []map[string]any {
	_ = model
	out := []map[string]any{}
	// onTaken 记录已占用"能力名"作为实例名的能力（每个能力只允许一个主实例）
	onTaken := map[string]bool{}
	for _, svc := range sp.Services {
		if svc.IID == 1 {
			continue
		}
		svcName := firstNonEmptyStr(svc.Name, humanizeURN(svc.Type))
		// 实例名前缀：服务短名（speaker / microphone / play-control…），
		// 避免不同服务下的同名属性/动作撞车。
		svcIID := actionNameOf(svc.Type)
		for _, pr := range svc.Properties {
			if !hasAccess(pr.Access, "write") {
				continue
			}
			var capName string
			switch pr.Format {
			case "bool":
				capName = "switch"
			case "uint8", "uint16", "uint32", "float":
				capName = "number"
			default:
				continue
			}
			/*
			 * instance_id 必须**全局唯一**（2026-11-17 实测配对失败）。
			 *
			 * 平台 `device_capabilities` 的唯一键是
			 * (device_id, endpoint_id, type, instance_id)。而同一台音箱的规格里
			 * **有两个 mute**：service 2「音箱静音」与 service 4「麦克风静音」——
			 * 只按属性名取实例名会撞车，配对直接 500：
			 * `UNIQUE constraint failed: device_capabilities…(2067)`。
			 *
			 * 所以用 `服务名_属性名`：speaker_mute / microphone_mute，既唯一又可读。
			 */
			propName := actionNameOf(pr.Type)
			/*
			 * ⚠ **主开关（属性名 `on`）的实例名必须保持为能力名本身**（2026-11-17）。
			 *
			 * 事故：台灯原来有 `switch`（主开关）+ `indicator_light_on`（指示灯），
			 * 我加了"服务名_属性名"命名后主开关变成 `switch_on`，
			 * 而**平台上报状态时无法带 instance_id**
			 * （`pluginres.StateHandler.PublishPluginState` 只有 capability/property/value，
			 * 设备域统一记成 `instance_id = 能力名`）。
			 *
			 * 结果：能力实例叫 `switch_on`、状态行叫 `switch` ⇒ 前端
			 * `readProperty(rows,'switch','power','switch_on')` 严格匹配失败 ⇒
			 * 卡片永远显示「未知」，而设备明明在线且有值（用户截图指出）。
			 *
			 * `on` 是 MIoT 里"主开关"的通用属性名，把它还原成能力名，
			 * 状态与能力就再次对齐；其余属性（indicator_light / sleep_mode / mute …）
			 * 仍用 `服务名_属性名` 保证唯一。
			 */
			instName := svcIID + "_" + propName
			/*
			 * 只有**第一个** `on` 属性才用能力名（2026-11-17 实测配对 500）。
			 *
			 * 台灯的规格里有两个服务都带 `on`：service 2（主开关）与
			 * service 3（指示灯）。我上一版把两者都映射成 `switch` ⇒
			 * 撞上 `device_capabilities` 的唯一键
			 * `(device_id, endpoint_id, type, instance_id)` ⇒ 配对直接 500。
			 *
			 * 现在：主开关（第一个 on）→ `switch`（与状态行的 instance_id 对齐，
			 * 平台上报状态时无法带实例，只能按能力名记）；
			 * 指示灯 → `indicator_light_on`（保持「服务名_属性名」）。
			 */
			if propName == "on" && !onTaken[capName] {
				instName = capName
				onTaken[capName] = true
			}
			c := map[string]any{
				"capability":  capName,
				"instance_id": instName,
				"name":        firstNonEmptyStr(pr.Name, humanizeURN(pr.Type)),
				"group":       svcName,
				"format":      pr.Format,
				"write":       true,
			}
			if len(pr.ValueRange) >= 2 {
				c["min"] = pr.ValueRange[0]
				c["max"] = pr.ValueRange[1]
				if len(pr.ValueRange) >= 3 && pr.ValueRange[2] > 0 {
					c["step"] = pr.ValueRange[2]
				}
			}
			if pr.Unit != "" {
				c["unit"] = pr.Unit
			}
			out = append(out, c)
		}
		for _, a := range svc.Actions {
			act := actionNameOf(a.Type)
			// 动作同理加服务前缀：多个服务都可能有同名动作。
			out = append(out, map[string]any{
				"capability":  "button",
				"instance_id": svcIID + "_" + act,
				"name":        firstNonEmptyStr(a.Name, humanizeURN(a.Type)),
				"group":       svcName,
				"action":      act,
			})
		}
	}
	/*
	 * 同名控件加**服务名前缀**消歧（2026-11-17）。
	 *
	 * 实测台灯的两个 switch 都显示「开关」——`switch_on`（主开关）与
	 * `indicator_light_on`（指示灯）在中文翻译里都叫"开关"，用户根本分不清
	 * 点哪个会关灯、点哪个只会关指示灯。
	 *
	 * 参考实现（HA）的显示是「麦克风 静音」「智能音箱 睡眠模式」——
	 * 即 **服务名 + 属性名**。这里只对**重名**的加前缀，避免所有控件都变长：
	 *
	 *	台灯：  开关 → 「开关」，指示灯 → 「指示灯 开关」
	 *	音箱：  静音 → 「静音」，麦克风 → 「麦克风 静音」
	 */
	count := map[string]int{}
	for _, c := range out {
		if n, _ := c["name"].(string); n != "" {
			count[n]++
		}
	}
	for _, c := range out {
		n, _ := c["name"].(string)
		g, _ := c["group"].(string)
		if n == "" || g == "" || count[n] <= 1 {
			continue
		}
		if g != n {
			c["name"] = g + " " + n
			ctx.LogWarn("hx-mi 控件名消歧：%q → %q", n, c["name"])
		}
	}
	ctx.LogWarn("hx-mi specControls 完成：%d 个控件，重名统计=%v", len(out), count)
	return out
}

// actionNameOf 从 urn 里取最后一段名字：`urn:...:action:play-radio:…` → `play-radio`。
func actionNameOf(urn string) string {
	parts := strings.Split(urn, ":")
	for i, p := range parts {
		if (p == "action" || p == "property" || p == "service") && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// humanizeURN 兜底显示名：urn 里那段 `play-radio` → `Play Radio`。
func humanizeURN(urn string) string {
	n := actionNameOf(urn)
	if n == "" {
		return ""
	}
	return strings.Title(strings.ReplaceAll(n, "-", " "))
}

// firstNonEmptyStr 取第一个非空串。
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// projectSpecControls 从规格里推导"可写属性 / 动作"对应的平台能力名。
//
// # 为什么这么做（2026-11-17）
//
// 用户拿 HA 插件的截图对照：同一台小爱音箱Pro，那边有「麦克风静音、睡眠模式、
// 停止闹钟、播放电台、唤醒、随机播放」等 10 个命名控件，我们这边一个都没有。
// 根因不是规格缺数据（实测 9/10 都在规格里），而是**平台按能力类型渲染固定控件**：
// `switch` 只画一个"开关"、`button` 只画一个"按下"。
//
// 所以先把规格里的可写属性/动作投影成能力，界面才有位置挂：
//
//	· 可写 bool 属性（mute / sleep-mode）→ `switch` 实例
//	· 可写数值属性（volume）             → `number` 实例
//	· 动作（stop-alarm / play-radio …）  → `button` 实例
//
// 返回的是**能力名集合**（平台按名字去重），实例明细由 spec_json 承载。
func projectSpecControls(ctx *plugin.Context, model string) []string {
	typ, ok := lookupModelType(model)
	if !ok {
		return nil
	}
	sp, err := loadMIoTSpec(ctx, typ)
	if err != nil {
		return nil
	}
	var out []string
	for _, svc := range sp.Services {
		if svc.IID == 1 {
			continue // device-information：厂商/型号/固件，不是控件
		}
		for _, pr := range svc.Properties {
			if !hasAccess(pr.Access, "write") {
				continue
			}
			switch pr.Format {
			case "bool":
				out = appendUnique(out, "switch")
			case "uint8", "uint16", "uint32", "float":
				out = appendUnique(out, "number")
			}
		}
		if len(svc.Actions) > 0 {
			out = appendUnique(out, "button")
		}
	}
	return out
}

// appendUnique 去重追加。
func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// isSensorLikeModel 判断型号是否属于"只读传感器"（需要周期读取而不是等下命令）。
//
// 判据用型号里的稳定词根：sensor / magnet / motion / leak / smoke / gas ——
// 这些是小米命名的事实（`lumi.sensor_gas.mcn02`、`miaomiaoce.sensor_ht.t2`）。
func isSensorLikeModel(model string) bool {
	m := strings.ToLower(model)
	for _, k := range []string{"sensor", "magnet", "motion", "leak", "smoke", "gas"} {
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
}

// readLANSensorState 经**局域网**读一台传感器的可读属性，组装成能力结构状态。
//
// # 形状
//
// 规格里除 service 1（device-information：厂商/型号/固件）以外、带 `read` 权限的
// 属性都读；按格式分派：
//
//	· float（如 gas-concentration）   ⇒ `sensor {value, unit?}`
//	· 整型/布尔（如 status 报警等级）  ⇒ `binary_sensor {value}`
//
// 不在这里编语义（0=正常/1=报警）：数值本身就是设备真值，原样呈现比不呈现诚实；
// 语义映射等确认型号文档后再加。
func readLANSensorState(ctx *plugin.Context, d miioDevice, external string) (map[string]any, error) {
	token, _, err := ctx.VaultBlobOpenDevice("miio_token", external)
	if err != nil || len(token) != 16 {
		return nil, errors.New("设备 token Vault 不可用")
	}
	typ, ok := lookupModelType(d.Model)
	if !ok {
		return nil, fmt.Errorf("型号 %s 不在目录中（无规格可取属性）", d.Model)
	}
	sp, err := loadMIoTSpec(ctx, typ)
	if err != nil {
		return nil, fmt.Errorf("读取规格失败：%w", err)
	}
	type ref struct{ kind, unit string }
	refs := map[string]ref{}
	var params []map[string]any
	for _, svc := range sp.Services {
		if svc.IID == 1 {
			continue // device-information
		}
		for _, pr := range svc.Properties {
			if !hasAccess(pr.Access, "read") {
				continue
			}
			params = append(params, map[string]any{"did": d.ExternalID, "siid": svc.IID, "piid": pr.IID})
			kind := "binary_sensor"
			if pr.Format == "float" {
				kind = "sensor"
			}
			refs[fmt.Sprintf("%d.%d", svc.IID, pr.IID)] = ref{kind: kind, unit: pr.Unit}
		}
	}
	if len(params) == 0 {
		return nil, errors.New("规格里没有可读属性")
	}
	rows, err := miioBatchProperties(ctx, d, token, "get_properties", params)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, row := range rows {
		if code, ok := row["code"].(float64); ok && int(code) != 0 {
			continue
		}
		siid, _ := row["siid"].(float64)
		piid, _ := row["piid"].(float64)
		r, ok := refs[fmt.Sprintf("%d.%d", int(siid), int(piid))]
		if !ok {
			continue
		}
		switch v := row["value"].(type) {
		case float64:
			if r.kind == "sensor" {
				val := map[string]any{"value": v}
				if r.unit != "" {
					val["unit"] = r.unit
				}
				out["sensor"] = val
			} else {
				out["binary_sensor"] = map[string]any{"value": v}
			}
		case bool:
			out["binary_sensor"] = map[string]any{"value": v}
		}
	}
	return out, nil
}

// readSensorStateViaCloud 经云端读传感器的可读属性（Zigbee 子设备走网关时用）。
//
// 与 readLANSensorState 的映射规则完全一致（float → sensor{value,unit}、
// 整型/布尔 → binary_sensor{value}），只是取数通道换成云端
// `miotspec/prop/get`（params 是**数组**）。
func readSensorStateViaCloud(ctx *plugin.Context, d miioDevice, external,
	sessionID, ssecurity, deviceID string) (map[string]any, error) {
	typ, ok := lookupModelType(d.Model)
	if !ok {
		return nil, fmt.Errorf("型号 %s 不在目录中", d.Model)
	}
	sp, err := loadMIoTSpec(ctx, typ)
	if err != nil {
		return nil, fmt.Errorf("读取规格失败：%w", err)
	}
	type ref struct{ kind, unit string }
	refs := map[string]ref{}
	var params []map[string]any
	for _, svc := range sp.Services {
		if svc.IID == 1 {
			continue
		}
		for _, pr := range svc.Properties {
			if !hasAccess(pr.Access, "read") {
				continue
			}
			params = append(params, map[string]any{"did": d.ExternalID, "siid": svc.IID, "piid": pr.IID})
			kind := "binary_sensor"
			if pr.Format == "float" {
				kind = "sensor"
			}
			refs[fmt.Sprintf("%d.%d", svc.IID, pr.IID)] = ref{kind: kind, unit: pr.Unit}
		}
	}
	if len(params) == 0 {
		return nil, errors.New("规格里没有可读属性")
	}
	rows, err := cloudRequestMIoTSpec(ctx, sessionID, ssecurity, deviceID, "prop/get", params)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, row := range rows {
		if code, ok := row["code"].(float64); ok && int(code) != 0 {
			continue
		}
		siid, _ := row["siid"].(float64)
		piid, _ := row["piid"].(float64)
		r, ok := refs[fmt.Sprintf("%d.%d", int(siid), int(piid))]
		if !ok {
			continue
		}
		switch v := row["value"].(type) {
		case float64:
			if r.kind == "sensor" {
				val := map[string]any{"value": v}
				if r.unit != "" {
					val["unit"] = r.unit
				}
				out["sensor"] = val
			} else {
				out["binary_sensor"] = map[string]any{"value": v}
			}
		case bool:
			out["binary_sensor"] = map[string]any{"value": v}
		}
	}
	return out, nil
}

// hasAccess 判断规格里的 access 列表是否包含某权限。
func hasAccess(list []string, want string) bool {
	for _, a := range list {
		if strings.EqualFold(a, want) {
			return true
		}
	}
	return false
}

// statePollBudget 是单轮同步里最多读几台设备的状态。
//
// # 为什么需要它（2026-11-17 用户报「设备状态都是未知」）
//
// 原实现硬编码"每轮 1 台"：11 台设备 × 5 分钟一轮 ⇒ **55 分钟**才轮完一遍。
// 用户点开设备页看到的是十几小时前的旧值，或干脆没有值（显示"未知"）。
//
// 但也不能无上限 —— wasm 单线程 + 插件 30s 执行预算：
//
//	· 局域网直连读：实测 3~35ms/台（很快）
//	· 云端读：百毫秒~秒级，失败还要重试
//
// 取"设备数的 1/3、最少 2 台、最多 6 台"：
//
//	· 3 台设备 → 每轮 2 台（几乎立刻全覆盖）
//	· 11 台    → 每轮 3 台（约 3 轮 = 15 分钟覆盖一遍，比 55 分钟好 3.7 倍）
//	· 30 台    → 每轮 6 台（封顶，避免拖垮同步）
func statePollBudget(deviceCount int) int {
	n := deviceCount / 3
	if n < 2 {
		n = 2
	}
	if n > 6 {
		n = 6
	}
	return n
}
