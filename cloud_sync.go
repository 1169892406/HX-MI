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

	sessionID, ssecurity, deviceID, err := openCloudSession(ctx)
	if err != nil {
		recordSyncFailure(ctx, err)
		return summary, err
	}
	defer func() { _ = ctx.HTTPClose(sessionID) }()

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
