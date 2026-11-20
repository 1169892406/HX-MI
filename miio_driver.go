package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"homex.dev/homex/sdk/go/plugin"
)

type miioDevice struct {
	ExternalID      string   `json:"external_id"`
	Host            string   `json:"host"`
	TokenRef        string   `json:"token_ref,omitempty"`
	Model           string   `json:"model"`
	Name            string   `json:"name"`
	Capabilities    []string `json:"capabilities"`
	NumericDeviceID uint32   `json:"numeric_device_id,omitempty"`
	// Account 是把这台设备**同步进登记**的账号（2026-11-20 用户要求：局域网
	// 检查列表按账号过滤 —— 多账号时必须能筛）。单会话设计下它等于"最近一次
	// 成功同步的会话账号"；换账号重新同步不会清掉旧账号的设备，它们的
	// Account 保持原值 —— 正因如此过滤才有意义。旧登记没有该字段，解析后
	// 为空串 ⇒ 前端按"未知来源"处理（不参与过滤），下一次同步会补上。
	Account string `json:"account,omitempty"`
}
type miioCommand struct {
	ExternalID string `json:"external_id"`
	Action     string `json:"action"`
	Capability string `json:"capability"`
	// Params 是插件内部归一化后的参数袋。
	Params map[string]any `json:"params"`
	// Args 是宿主 pluginres.CommandRequest 的线上字段（resource_host.go 写
	// "args"）。早期插件只认 "params"，导致 set_property/set-brightness 这类
	// 带参命令全部拿到空参数（t234 实测：Binding 不支持 action=set_property）。
	// 二者都收，优先 params（内部/测试），缺省回落到 args（宿主真实下发）。
	Args map[string]any `json:"args"`
	/*
	 * InstanceID 是能力实例标识（`sleep_mode` / `volume` / `wake_up`…）。
	 *
	 * ⚠ 平台当前**不投递**这个字段（resource_host.go 只发 external_id/
	 * capability/action/args/request_id）—— 见 platformsGapNote。
	 * 所以本插件先从 `args` 里取同名键兜底（前端可以把实例名放进 parameters）。
	 * 待平台补上该字段后，这条路自动变成直读，不需要再改插件。
	 */
	InstanceID string `json:"instance_id"`
}

/*
 * platformsGapNote：平台设备命令缺少 instance_id 透传（2026-11-17 核实）。
 *
 * `internal/app/pluginsvc/resource_host.go` 的投递体只有
 * external_id / capability / action / args / request_id。于是"同一能力多实例"
 * （多个可写属性 = 多个 switch 实例、多个动作 = 多个 button 实例）在插件侧
 * **无法区分是哪一个** —— 只能靠 args 里带名字约定。
 *
 * 这是平台侧的窄口子，不是插件能自行补齐的；本插件先用 args 约定兜住功能。
 */
func commandInstanceID(cmd miioCommand) string {
	if v := strings.TrimSpace(cmd.InstanceID); v != "" {
		return v
	}
	for _, k := range []string{"instance_id", "instance", "property", "action_name"} {
		if raw, ok := cmd.Params[k]; ok {
			if s, ok := raw.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		if raw, ok := cmd.Args[k]; ok {
			if s, ok := raw.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func miioPKCS7(b []byte) []byte {
	n := aes.BlockSize - len(b)%aes.BlockSize
	return append(b, bytes.Repeat([]byte{byte(n)}, n)...)
}
func buildMIIOPacket(token []byte, deviceID, ts, id uint32, method string, params any) ([]byte, error) {
	if len(token) != 16 {
		return nil, errors.New("miIO token 必须16字节")
	}
	plain, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	plain = append(plain, 0)
	key := md5.Sum(token)
	iv := md5.Sum(append(append([]byte{}, key[:]...), token...))
	block, _ := aes.NewCipher(key[:])
	p := miioPKCS7(plain)
	enc := make([]byte, len(p))
	cipher.NewCBCEncrypter(block, iv[:]).CryptBlocks(enc, p)
	raw := make([]byte, 32, 32+len(enc))
	raw[0] = 0x21
	raw[1] = 0x31
	binary.BigEndian.PutUint16(raw[2:4], uint16(32+len(enc)))
	binary.BigEndian.PutUint32(raw[8:12], deviceID)
	binary.BigEndian.PutUint32(raw[12:16], ts)
	h := md5.New()
	h.Write(raw[:16])
	h.Write(token)
	h.Write(enc)
	copy(raw[16:32], h.Sum(nil))
	return append(raw, enc...), nil
}
func handleDriverCommand(ctx *plugin.Context, payload json.RawMessage) error {
	var cmd miioCommand
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return err
	}
	if len(cmd.Params) == 0 && len(cmd.Args) > 0 {
		cmd.Params = cmd.Args
	}
	devices, err := loadDeviceRegistry(ctx)
	if err != nil {
		return err
	}
	var d *miioDevice
	for i := range devices {
		if devices[i].ExternalID == cmd.ExternalID {
			d = &devices[i]
			break
		}
	}
	if d == nil {
		return errors.New("设备未登记")
	}
	/*
	 * 云端设备分支（2026-11-17）：`host` 为空的设备（红外遥控这类）没有局域网
	 * 地址，LAN 路径必然失败，而且它们连 miio_token 都没有 —— 所以必须在
	 * `resolveCommandBinding` 与 token 读取**之前**分流。
	 *
	 * 下发方式：`miotspec/action` 的 turn-on / turn-off（按规格找 aiid）。
	 * 本轮只做开关：先验证"红外到底能不能动作"，风速/温度用同一通路再扩。
	 */
	/*
	 * 媒体播放器分支（2026-11-17，第 2 项）：**走云端**。
	 *
	 * 小爱音箱Pro 虽是 Wi-Fi 设备、`devices.json` 里也有 host+token，但实测
	 * 局域网 miio 直连直接超时（30s 打满、沙箱被打死）—— 音箱的本地口不通，
	 * 这也是参考实现把它们归到云端 action 的原因。
	 *
	 * 规格（xiaomi-lx06，已缓存）：
	 *   svc2 volume(uint8) / mute(bool)
	 *   svc3 actions play/pause/next/previous + playing-state(uint8)
	 *   svc5 actions wake-up / play-music / play-text
	 */
	/*
	 * 规格驱动的通用控制（2026-11-17）：`button` / `number` / 额外 `switch` 实例。
	 *
	 * 这些能力不是固定语义，而是**规格里的某个动作或可写属性**：
	 *   button + action(mute/stop_alarm/play_radio/wake_up…) → 调对应 action
	 *   number + set_value(volume)                          → 写对应属性
	 *   switch + instance_id(如 sleep_mode)                 → 写该 bool 属性
	 *
	 * 平台发的命令 shape 由前端 capabilityControls 决定：
	 *   button → action:"press"，我们按 capability 的 spec 元数据找动作名（见下）
	 *   number → action:"set_value" + parameters.value
	 *   switch → action:"turn_on"/"turn_off"（沿用开关语义）
	 *
	 * 因为一个设备可能有多个 button/number 实例，前端会把 instance_id 带上，
	 * 我们据此定位规格里的具体动作/属性。
	 */
	if hasSpecDrivenCap(d.Capabilities, cmd) {
		if err := execSpecDriven(ctx, *d, cmd); err != nil {
			return err
		}
		return nil
	}
	if hasCap(d.Capabilities, "media_player") {
		if err := cloudMediaCommand(ctx, *d, cmd); err != nil {
			return err
		}
		/*
		 * ⚠ **不在命令后立刻回读**（2026-11-17 实测）。
		 *
		 * 云端状态滞后整整一轮：
		 *
		 *	设 0.2 → 立刻回读 0.7（上一轮的值）
		 *	设 0.8 → 立刻回读 0.2（上一轮的值）
		 *
		 * 参考实现也是这么处理的：写完先 `asyncio.sleep(delay_update)` 再读
		 * （init.py:1042 的 `_vars['delay_update']`，默认值见 device_customizes
		 * 的 `cloud_delay_update: 7~10`）。立刻回读只会把**旧值**写进设备域，
		 * 界面上看起来像"命令被还原了"——比不显示更糟。
		 *
		 * 所以：绝对值写入（prop/set 回 code=0 = 设备**接受**了这个值）上报写入值；
		 * 其余状态（muted / playing）交给周期轮询（见 cloud_sync 的 poll 分支），
		 * 那时云端的快照已经追上来了。
		 */
		if lv, ok := mediaAcknowledgedVolume(cmd); ok {
			_ = ctx.DeviceStatePublish(plugin.DeviceStateRequest{
				ExternalID: d.ExternalID,
				State:      map[string]any{"media_player": map[string]any{"volume_level": lv}},
				ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
			})
		}
		return nil
	}
	if strings.TrimSpace(d.Host) == "" {
		switch strings.ToLower(cmd.Action) {
		case "turn_on", "turn_off":
			on := strings.EqualFold(cmd.Action, "turn_on")
			if err := cloudExecuteIRPower(ctx, *d, on); err != nil {
				return err
			}
			/*
			 * 不伪造状态：红外没有可回读的开关属性（规格里只有 ir-mode /
			 * ir-temperature），把"我发了 turn_on"当"设备已开"就是乐观确认。
			 * 平台侧该命令会以 acked=false 呈现 —— 这是诚实的。
			 */
			return nil
		case "set_temperature":
			/*
			 * 平台 climate 的滑块发 `set_temperature` + `target_temp_c`
			 * （见前端 capabilityControls.ts:16-30）。红外空调把这个值写到
			 * `ir-temperature` 属性即可 —— 规格里它的范围就是 16~30、步长 1，
			 * 所以先按规格夹紧，避免发出设备不接受的值。
			 */
			raw, ok := cmd.Params["target_temp_c"]
			if !ok {
				raw = cmd.Params["temperature"]
			}
			temp, tok := cloudNumber(raw)
			if !tok {
				return fmt.Errorf("温度参数不可用（收到 %v）", raw)
			}
			if temp < 16 {
				temp = 16
			}
			if temp > 30 {
				temp = 30
			}
			if err := cloudIRSetProp(ctx, *d, ":ir-temperature:", temp); err != nil {
				return err
			}
			return nil
		case "set_mode":
			raw, _ := cmd.Params["mode"].(string)
			mode, ok := irModeValue(raw)
			if !ok {
				return fmt.Errorf("不支持的模式 %q（可用 auto/cool/dry/heat/fan_only）", raw)
			}
			if err := cloudIRSetProp(ctx, *d, ":ir-mode:", mode); err != nil {
				return err
			}
			return nil
		case "set_fan_mode", "set_swing_mode":
			/*
			 * 风速/导风：规格里只有 `fan-speed-up` / `fan-speed-down` **相对动作**，
			 * 没有"设到某一档"的属性 —— 用相对动作去凑绝对值会连发好几条红外指令，
			 * 而且中间状态不可知。宁可明确说不支持，也不做这种"看起来能调"的实现。
			 */
			return fmt.Errorf("红外遥控暂不支持 %s（规格只有 fan-speed-up/down 相对动作）", cmd.Action)
		default:
			return fmt.Errorf("红外遥控暂只支持开关/温度/模式（收到 %q）", cmd.Action)
		}
	}
	cmd, err = resolveCommandBinding(ctx, *d, cmd)
	if err != nil {
		return err
	}
	/*
	 * 命令优先通道（2026-11-17 用户要求二选一，`lan_priority`）：
	 *   lan   —— 局域网优先（默认）：走下面的 LAN 直连，失败**不自动降级**
	 *            到云端（LAN 失败多为设备离线，云端也大概率不通；
	 *            而且静默降级会让"局域网更快"这个承诺变成谎言）；
	 *   cloud —— 云端优先：这里就转云端，成功即返回；云端不支持该动作时
	 *            再落到下面走局域网（"优先"而非"只用"）。
	 *
	 * ⚠ 此前 `lan_priority` 只是**存了配置项**、路由完全没读 —— 用户实测
	 * 「和自动同步没有区别」正是这个原因。现在真正生效。
	 */
	if strings.EqualFold(current.LanPriority, "cloud") {
		handled, cerr := tryCloudCommand(ctx, *d, cmd)
		if handled && cerr == nil {
			return nil
		}
		if handled && cerr != nil {
			// 云端明确失败：记录后继续尝试局域网（体现"优先"语义）
			ctx.LogWarn("hx-mi 云端优先通道失败，回退局域网（设备 %s）：%v", d.ExternalID, cerr)
		}
		// 云端不支持该动作 ⇒ handled=false，直接落到局域网路径
	}
	token, _, err := ctx.VaultBlobOpenDevice("miio_token", d.ExternalID)
	if err != nil || len(token) != 16 {
		return errors.New("设备 token Vault 不可用")
	}
	method, params, err := mapControl(cmd)
	if err != nil {
		return err
	}
	state := map[string]any{}
	if isLegacyDevice(ctx, *d, token) {
		switch strings.ToLower(cmd.Action) {
		case "turn_on", "turn_off":
			want := "on"
			if strings.EqualFold(cmd.Action, "turn_off") {
				want = "off"
			}
			if _, err := miioExchange(ctx, *d, token, "set_power", []string{want}); err != nil {
				return err
			}
		case "toggle":
			// 老协议也有 toggle 语义的设备，但不通用：先读再写更可靠。
			cur, err := legacyGetProp(ctx, *d, token, []string{"power"})
			if err != nil {
				return err
			}
			on, ok := legacyPowerValue(cur["power"])
			if !ok {
				return errors.New("toggle 当前值不可用")
			}
			want := "on"
			if on {
				want = "off"
			}
			if _, err := miioExchange(ctx, *d, token, "set_power", []string{want}); err != nil {
				return err
			}
		default:
			// 其它动作（亮度/模式等）不属于本次范围：保持原路径。
		}
		// 回读真值：power 一定有；temperature 有就一起上报。
		got, gerr := legacyGetProp(ctx, *d, token, []string{"power", "temperature"})
		if gerr == nil {
			if on, ok := legacyPowerValue(got["power"]); ok {
				state["switch"] = map[string]any{"power": on}
			}
			if t, ok := legacyNumber(got["temperature"]); ok && t > -40 && t < 80 {
				/*
				 * ⚠ 统一写 `temperature`（2026-11-19 用户「以本地为准」）。
				 *
				 * 历史教训：老协议这条路写 `value`，而规格化那条路写 `temperature`，
				 * 平台状态库是**按属性名合并**的（repo.applyStatePatches 只覆盖同名键，
				 * 从不删除别的键）⇒ 同一个温度在库里留下两个键，界面上出现
				 * 「温度 37」和「数值 40」两个数，其中一个还是两天前的残留。
				 *
				 * 现在两条本地路径都写 `temperature`（+ `unit`），键名一致就不会再分裂。
				 */
				state["sensor"] = map[string]any{"temperature": t, "unit": "℃"}
			}
		} else {
			ctx.LogWarn("hx-mi 老协议状态回读失败（设备 %s）：%v", d.ExternalID, gerr)
		}
		if len(state) == 0 {
			return nil
		}
		return ctx.DeviceStatePublish(plugin.DeviceStateRequest{ExternalID: d.ExternalID, State: state, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	}
	if method == "set_properties" || method == "get_properties" || method == "action" {
		if list, ok := params.([]map[string]any); ok {
			rows, e := miioBatchProperties(ctx, *d, token, method, list)
			if e != nil {
				return e
			}
			if method == "action" {
				decoded, e := decodeActionOutputs(firstRow(rows), toIntSlice(cmd.Params["out"]))
				if e != nil {
					return e
				}
				state["action_out"] = decoded
			}
		}
	} else {
		if _, err = miioExchange(ctx, *d, token, method, params); err != nil {
			return err
		}
	}
	// 开关态：直接用命令意图（set_properties 已成功返回，说明设备接受了）。
	if power, ok := commandPowerIntent(cmd.Action); ok {
		state["switch"] = map[string]any{"power": power}
	}
	// 温度：本机型号规格里的可选属性，读不到就跳过（不拖累命令结果）。
	if t, ok := readTemperatureProperty(ctx, *d, token); ok {
		/* 带上 unit，与老协议那条同形（见上面注释：键名必须一致）。 */
		state["sensor"] = map[string]any{"temperature": t, "unit": "℃"}
	}
	if len(state) == 0 {
		return nil
	}
	return ctx.DeviceStatePublish(plugin.DeviceStateRequest{ExternalID: d.ExternalID, State: state, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)})
}

// commandPowerIntent 把"开关类动作"翻成 power 布尔值。
//
// toggle 不给值：它真正的结果要靠随后读属性才知道，这里不猜。
func commandPowerIntent(action string) (bool, bool) {
	switch strings.ToLower(action) {
	case "turn_on":
		return true, true
	case "turn_off":
		return false, true
	}
	return false, false
}

/*
 * 老 miio 协议支持（2026-11-16，用户给的参考实现 hass-xiaomi-miot 定案）。
 *
 * # 为什么必须支持它
 *
 * 这台 `chuangmi.plug.m3` 在参考项目里是**老 miio 协议**设备：
 *
 *	'chuangmi.plug.m3': 'chuangmi.plug.m1'   → extend_model chuangmi.plug.hmi205
 *	'miio_specs': {
 *	    'prop.2.1': {'prop': 'power', 'setter': True},   # 用 set_power
 *	    'prop.2.2': {'prop': 'temperature'},            # 用 get_prop "temperature"
 *	}
 *
 * 也就是说：它的"功率/温度"是**老协议的字符串属性名**，要走
 * `get_prop ["power","temperature"]` 与 `set_power ["on"/"off"]`，
 * 而不是 MIoT 的 `get_properties(siid/piid)` —— 后者在它身上只会返回
 * `code:-30001 Resp invalid json`（实测）或莫名其妙的值。
 *
 * # 顺带修掉一个更严重的问题
 *
 * 之前上报的开关状态来自**命令意图**（turn_on ⇒ power=true），而不是设备回读。
 * 设备若没真的动作，界面照样显示成功 —— 用户报的"点了没反应但看着像成功"
 * 正是这么来的。老协议路径下我们改从 `get_prop` **回读**，状态才是设备真值。
 *
 * # 探测与缓存
 *
 * 一个设备属于哪种协议，由设备自己回答：先试 `get_prop ["power"]`，
 * 拿到数组即老协议；结果按 externalID 缓存到 FS（`miio-protocol.json`），只探一次。
 */
const miioProtocolPath = "miio-protocol.json"

// loadLegacyDevices 读"老协议设备"缓存（缺失/损坏当空，会重探一次）。
func loadLegacyDevices(ctx *plugin.Context) map[string]bool {
	out := map[string]bool{}
	r, err := ctx.FSRead(miioProtocolPath)
	if err != nil || !r.Found {
		return out
	}
	b, derr := r.Data()
	if derr != nil {
		return out
	}
	_ = json.Unmarshal(b, &out)
	return out
}

func saveLegacyDevices(ctx *plugin.Context, m map[string]bool) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	w := plugin.FSWriteRequest{Path: miioProtocolPath, QuotaKB: 256}
	w.SetData(b)
	if err := ctx.FSWrite(w); err != nil {
		ctx.LogWarn("hx-mi 协议缓存写入失败（不影响控制）：%v", err)
	}
}

// legacyGetProp 用老协议读若干属性，返回按名字对齐的值。
//
// 老协议返回 `{"result":[...]}`，顺序与请求一致（这是 miio 的老约定）。
func legacyGetProp(ctx *plugin.Context, d miioDevice, token []byte, names []string) (map[string]any, error) {
	env, err := miioExchange(ctx, d, token, "get_prop", names)
	if err != nil {
		return nil, err
	}
	list, ok := env["result"].([]any)
	if !ok {
		return nil, errors.New("get_prop 返回不是数组")
	}
	out := map[string]any{}
	for i, n := range names {
		if i < len(list) {
			out[n] = list[i]
		}
	}
	return out, nil
}

// isLegacyDevice 判断该设备是否走老 miio 协议（结果缓存）。
func isLegacyDevice(ctx *plugin.Context, d miioDevice, token []byte) bool {
	cache := loadLegacyDevices(ctx)
	if v, ok := cache[d.ExternalID]; ok {
		return v
	}
	_, err := legacyGetProp(ctx, d, token, []string{"power"})
	legacy := err == nil
	cache[d.ExternalID] = legacy
	saveLegacyDevices(ctx, cache)
	if legacy {
		ctx.LogWarn("hx-mi 该设备走老 miio 协议（device=%s model=%s）：用 get_prop/set_power", d.ExternalID, d.Model)
	}
	return legacy
}

// legacyPowerValue 把老协议的 on/off 值翻成布尔。
func legacyPowerValue(v any) (bool, bool) {
	switch t := v.(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(t), "on"), true
	case bool:
		return t, true
	}
	return false, false
}

// legacyNumber 把老协议返回的温度（可能是字符串/数字）转成浮点。
func legacyNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err == nil {
			return f, true
		}
	}
	return 0, false
}

/*
 * 温度属性绑定（2026-11-16 用户选 A 方案：**属性枚举探测**）。
 *
 * # 为什么不能写死 siid/piid
 *
 * 之前的假设是「插座类设备开关服务 siid=2 的 piid=2 就是 temperature」——
 * 那是从**缓存的 MIoT 规格**读来的。实测这台 chuangmi.plug.m3 对
 * `siid=2,piid=2` 返回的是 `true`（布尔），即**规格与该固件不一致**：
 *
 *	plugin: hx-mi 温度值类型异常（设备 237970870 value=true）
 *
 * 型号表/规格表都会与实机漂移，所以改成**问设备本身**：小范围枚举
 * (siid 2..4) × (piid 1..3)，挑出「返回数值且在合理温区」的那个属性。
 *
 * # 三条自律
 *
 *  1. **只探一次**：结果按设备 externalID 缓存在插件 FS（`temperature-bindings.json`），
 *     重启后仍然有效。否则每条命令多 9 次属性读，LAN 上就是噪音。
 *  2. **绝不影响命令**：探测失败只是"这台没有温度"，开关已经切过去了，
 *     不能因为温度读不到就让用户以为没开成。
 *  3. **判据要能解释**：挑中的属性连同依据一起记 Warn 日志
 *     （设备、siid、piid、值），排查时不用再猜。
 */
type tempBinding struct {
	SIID int `json:"siid"`
	PIID int `json:"piid"`
}

// temperaturePlausible 判断一个读数是否像温度（摄氏度）。
//
// 范围取得宽：-40~80℃ 覆盖家用场景（含冷冻柜与阳台暴晒），
// 又不至于把开关/亮度/功率这类整数属性误认成温度。
const (
	tempProbeSiidMin = 2
	tempProbeSiidMax = 4
	tempProbePiidMin = 1
	tempProbePiidMax = 3
)

const tempBindingsPath = "temperature-bindings.json"

// loadTempBindings 读缓存（缺失/损坏都当空，探测会重来一次）。
func loadTempBindings(ctx *plugin.Context) map[string]tempBinding {
	out := map[string]tempBinding{}
	r, err := ctx.FSRead(tempBindingsPath)
	if err != nil || !r.Found {
		return out
	}
	b, derr := r.Data()
	if derr != nil {
		return out
	}
	_ = json.Unmarshal(b, &out)
	return out
}

// saveTempBindings 落盘。失败只 Warn：缓存丢了最多下次再探一遍。
func saveTempBindings(ctx *plugin.Context, m map[string]tempBinding) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	w := plugin.FSWriteRequest{Path: tempBindingsPath, QuotaKB: 256}
	w.SetData(b)
	if err := ctx.FSWrite(w); err != nil {
		ctx.LogWarn("hx-mi 温度绑定缓存写入失败（不影响控制）：%v", err)
	}
}

// resolveTemperatureBinding 取该设备的温度属性：缓存优先，没有就探一次。
func resolveTemperatureBinding(ctx *plugin.Context, d miioDevice, token []byte) (tempBinding, bool) {
	cache := loadTempBindings(ctx)
	if b, ok := cache[d.ExternalID]; ok && b.SIID > 0 && b.PIID > 0 {
		return b, true
	}
	b, ok := probeTemperatureBinding(ctx, d, token)
	if !ok {
		return tempBinding{}, false
	}
	cache[d.ExternalID] = b
	saveTempBindings(ctx, cache)
	return b, true
}

// probeTemperatureBinding 枚举一小片属性，挑出像温度的那个。
//
// 一次 get_properties 打包 9 个 (siid,piid) —— miioBatchProperties 内部按 15 个
// 一包，所以这里只发一包，代价可控。
func probeTemperatureBinding(ctx *plugin.Context, d miioDevice, token []byte) (tempBinding, bool) {
	var params []map[string]any
	for siid := tempProbeSiidMin; siid <= tempProbeSiidMax; siid++ {
		for piid := tempProbePiidMin; piid <= tempProbePiidMax; piid++ {
			params = append(params, map[string]any{"did": d.ExternalID, "siid": siid, "piid": piid})
		}
	}
	/*
	 * ⚠ **逐个读，不要整包发**（2026-11-16 实测）。
	 *
	 * 先按"一次 get_properties 打包 9 个属性"实现，这台 chuangmi.plug.m3 直接回
	 * `code:-30001 Resp invalid json` —— 它不支持一次问这么多属性（廉价的 miio
	 * 固件很常见：单属性可读，批量就吐坏响应）。而整包失败会把"能读到的那个"
	 * 一起丢掉，于是永远探不出来。
	 *
	 * 改成逐个探测：
	 *  · 单属性读是这设备**已经证明支持**的路径（开关命令就是 set_properties 单项）；
	 *  · 某个 siid/piid 不支持时只跳过它自己，不影响其它候选；
	 *  · 探测只在**首次**进行（结果按设备缓存），9 次读不会变成常态开销。
	 *
	 * 参考实现 hass-xiaomi-miot 也是这个取向：按规格逐项取、单项失败不连坐整批。
	 */
	rows := make([]map[string]any, 0, len(params))
	for _, one := range params {
		got, err := miioBatchProperties(ctx, d, token, "get_properties", []map[string]any{one})
		if err != nil {
			// 单个属性读不到是常态（这台设备就不支持这个 siid/piid），
			// 不记 Warn 以免刷屏；整轮都失败才在下面统一说明。
			continue
		}
		rows = append(rows, got...)
	}
	if len(rows) == 0 {
		ctx.LogWarn("hx-mi 温度属性探测无任何响应（设备 %s model=%s，已逐项枚举 siid %d-%d × piid %d-%d）",
			d.ExternalID, d.Model, tempProbeSiidMin, tempProbeSiidMax, tempProbePiidMin, tempProbePiidMax)
		return tempBinding{}, false
	}
	for _, row := range rows {
		if code, ok := row["code"].(float64); ok && int(code) != 0 {
			continue // 该属性这台设备不支持
		}
		// 只认**数值**：布尔（开关）与字符串（型号/固件）一律跳过。
		v, isNum := row["value"].(float64)
		if !isNum {
			continue
		}
		if v < -40 || v > 80 {
			continue
		}
		siid, _ := row["siid"].(float64)
		piid, _ := row["piid"].(float64)
		if siid <= 0 || piid <= 0 {
			continue
		}
		b := tempBinding{SIID: int(siid), PIID: int(piid)}
		ctx.LogWarn("hx-mi 温度属性已定位（设备 %s model=%s）：siid=%d piid=%d 当前值=%v",
			d.ExternalID, d.Model, b.SIID, b.PIID, v)
		return b, true
	}
	ctx.LogWarn("hx-mi 该设备没有可用的温度属性（设备 %s model=%s，已枚举 siid %d-%d × piid %d-%d）",
		d.ExternalID, d.Model, tempProbeSiidMin, tempProbeSiidMax, tempProbePiidMin, tempProbePiidMax)
	return tempBinding{}, false
}

// readTemperatureProperty 读该设备的温度值（属性位置由探测决定）。
func readTemperatureProperty(ctx *plugin.Context, d miioDevice, token []byte) (float64, bool) {
	b, ok := resolveTemperatureBinding(ctx, d, token)
	if !ok {
		return 0, false
	}
	rows, err := miioBatchProperties(ctx, d, token, "get_properties",
		[]map[string]any{{"did": d.ExternalID, "siid": b.SIID, "piid": b.PIID}})
	if err != nil {
		ctx.LogWarn("hx-mi 温度读取失败（设备 %s siid=%d piid=%d）：%v", d.ExternalID, b.SIID, b.PIID, err)
		return 0, false
	}
	row := firstRow(rows)
	if row == nil {
		ctx.LogWarn("hx-mi 温度读取返回空（设备 %s）", d.ExternalID)
		return 0, false
	}
	if code, ok := row["code"].(float64); ok && int(code) != 0 {
		ctx.LogWarn("hx-mi 温度读取被设备拒绝（设备 %s code=%d siid=%d piid=%d）",
			d.ExternalID, int(code), b.SIID, b.PIID)
		return 0, false
	}
	switch v := row["value"].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	}
	ctx.LogWarn("hx-mi 温度值类型异常（设备 %s value=%v）", d.ExternalID, row["value"])
	return 0, false
}

func mapControl(c miioCommand) (string, any, error) {
	switch strings.ToLower(c.Action) {
	case "turn_on":
		return "set_properties", []map[string]any{{"did": c.ExternalID, "siid": 2, "piid": 1, "value": true}}, nil
	case "turn_off":
		return "set_properties", []map[string]any{{"did": c.ExternalID, "siid": 2, "piid": 1, "value": false}}, nil
	case "toggle":
		return "toggle", c.Params, nil
	case "set_property":
		p := map[string]any{}
		for k, v := range c.Params {
			p[k] = v
		}
		if metaRaw, ok := p["meta"].(map[string]any); ok {
			meta := propertyBinding{}
			b, _ := json.Marshal(metaRaw)
			_ = json.Unmarshal(b, &meta)
			encoded, e := encodeProperty(meta, p["value"], fmt.Sprint(p["encode"]))
			if e != nil {
				return "", nil, e
			}
			p["value"] = encoded
			delete(p, "meta")
			delete(p, "encode")
		}
		return "set_properties", []map[string]any{p}, nil
	case "action":
		p, e := buildActionParams(c)
		if e != nil {
			return "", nil, e
		}
		return "action", []map[string]any{p}, nil
	default:
		return "", nil, fmt.Errorf("不支持动作 %s", c.Action)
	}
}
func base64String(b []byte) string { return encodeBase64(b) }
func encodeBase64(b []byte) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out strings.Builder
	for i := 0; i < len(b); i += 3 {
		var v uint32
		remain := len(b) - i
		v = uint32(b[i]) << 16
		if remain > 1 {
			v |= uint32(b[i+1]) << 8
		}
		if remain > 2 {
			v |= uint32(b[i+2])
		}
		out.WriteByte(table[(v>>18)&63])
		out.WriteByte(table[(v>>12)&63])
		if remain > 1 {
			out.WriteByte(table[(v>>6)&63])
		} else {
			out.WriteByte('=')
		}
		if remain > 2 {
			out.WriteByte(table[v&63])
		} else {
			out.WriteByte('=')
		}
	}
	return out.String()
}

// cloudReadSensorState 经**云端**读一台 BLE 设备（温湿度计这类没有 LAN IP 的设备）。
//
// # 为什么必须走云端
//
// BLE 传感器没有 IP（`devices.json` 里 host 为空），本地 miio 的 hello/加密通道
// 对它无从下手；读它只有两条路：小米多模网关转发，或云端 `miotspec/prop/get`。
// 本插件已经有可用的云会话（同步设备清单用的就是它），所以走云端最省。
//
// # 只读这三项
//
// `miaomiaoce.sensor_ht.t2` 的规格（本地 spec-cache 核对过）：
//
//	siid=2 piid=1 → temperature
//	siid=2 piid=2 → relative-humidity
//	siid=3 piid=1 → battery-level
//
// # 上报形状
//
// 前端只认 `sensor.value` + `sensor.unit` 两个属性（见 sensorReadouts），
// 所以主读数发 value/unit；湿度额外带一个 `humidity` 属性
// （当前卡片不显示，但数据在，将来做湿度卡不用重跑链路）。
func cloudReadSensorState(ctx *plugin.Context, sessionID, ssecurity, deviceID, did string) (map[string]any, error) {
	type propRef struct {
		SIID int
		PIID int
	}
	refs := []propRef{{2, 1}, {2, 2}, {3, 1}}
	params := make([]map[string]any, 0, len(refs))
	for _, r := range refs {
		params = append(params, map[string]any{"did": did, "siid": r.SIID, "piid": r.PIID})
	}
	/*
	 * ⚠ **不要带 `datasource`**（2026-11-17 按参考实现 hass-xiaomi-miot 核对）。
	 *
	 * 该项目的 `async_request_miot_spec` 只发 `{"params": [...]}`：
	 *
	 *	async def async_request_miot_spec(self, api, params=None, **kwargs):
	 *	    resp = await self.async_request_api('miotspec/' + api, {'params': params or []}, ...)
	 *
	 * 而我之前自己加了 `"datasource": 2`，云端对 BLE 设备回 `code:-704010000`
	 * （设备离线），看起来像"设备不在线"，实际是这个字段让请求走了另一条取数路径。
	 * 按参考实现去掉，只发 params。
	 */
	body, err := json.Marshal(map[string]any{"params": params})
	if err != nil {
		return nil, err
	}
	raw, err := apiPostRC4GuestDevice(ctx, sessionID, ssecurity, deviceID,
		"https://api.io.mi.com/app/miotspec/prop/get", string(body))
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result []struct {
			SIID  int `json:"siid"`
			PIID  int `json:"piid"`
			Value any `json:"value"`
			Code  int `json:"code"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, errors.New("云端属性响应解析失败")
	}
	out := map[string]any{}
	for _, r := range resp.Result {
		if r.Code != 0 {
			continue
		}
		num, ok := cloudNumber(r.Value)
		if !ok {
			continue
		}
		switch {
		case r.SIID == 2 && r.PIID == 1:
			/*
			 * 云端正名 `temperature`（2026-11-19 与两条本地路径对齐）。
			 *
			 * 这条路只走 **BLE 传感器**（没有局域网 IP，云端是唯一来源，见
			 * cloud_sync.go 的 `LocalIP == "" && model 含 sensor` 判据），
			 * 所以"以本地为准"在这里不冲突：能本地读的设备根本进不到这里。
			 * 键名与本地一致，同一个量才不会在库里出现两个键。
			 */
			out["sensor"] = map[string]any{"temperature": num, "unit": "℃"}
		case r.SIID == 2 && r.PIID == 2:
			if s, ok := out["sensor"].(map[string]any); ok {
				s["humidity"] = num
			} else {
				out["sensor"] = map[string]any{"humidity": num}
			}
		case r.SIID == 3 && r.PIID == 1:
			if s, ok := out["sensor"].(map[string]any); ok {
				s["battery"] = num
			}
		}
	}
	if len(out) == 0 {
		/*
		 * 设备离线要**如实说**，别说成"没有可用读数"（2026-11-16 实测原文）：
		 *
		 *	{"code":0,"message":"ok","result":[
		 *	  {"did":"blt.3.152hiadjgec01","siid":2,"piid":1,"code":-704010000},
		 *	  ... 全部 -704010000 ]}
		 *
		 * `-704010000` 是小米云的"设备离线/不可达"（BLE 传感器在休眠很常见）。
		 * 把它当成"读取失败"会让人去查插件；如实写"设备离线"才会让人去查设备。
		 */
		offline := len(resp.Result) > 0
		for _, r := range resp.Result {
			if r.Code != -704010000 {
				offline = false
				break
			}
		}
		if offline {
			return nil, errors.New("设备离线（云端 code=-704010000：BLE 传感器休眠或不在网关范围内）")
		}
		// 其它情况把原始响应记下来：不打出原文就只能猜（本仓反复踩过的坑）。
		preview := string(raw)
		if len(preview) > 400 {
			preview = preview[:400] + "…"
		}
		return nil, fmt.Errorf("云端没有返回可用读数（原始响应：%s）", preview)
	}
	return out, nil
}

// cloudNumber 把云端返回的数值（数字或字符串）统一成 float64。
func cloudNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err == nil {
			return f, true
		}
	}
	return 0, false
}

// —— 云端写通道（红外设备没有局域网地址，只能经云端；2026-11-17）——
//
// 用户口径：「遥控数据已经配置并且存在本地，那么离线可以控制」——那是小爱音箱
// 自己的行为（遥控码存在音箱里）。**平台要发指令仍然得通过音箱**，而音箱只接受
// 云端 `miotspec/*` 或本地 miio 两根入口。参考实现 hass-xiaomi-miot 走的是云端：
//
//	async_set_props  → miotspec/prop/set
//	async_do_action  → miotspec/action
//
// 所以本轮先把云端写通道补上；本地 miio 红外方法留待下一步探测（方法名未知，
// 不靠猜写代码）。

// cloudRequestMIoTSpec 调云端 miotspec/<api>（prop/set、action、prop/get 共用）。
//
// 与参考实现一致：body **只有 params**（不要 datasource，这个字段曾让 BLE
// 读取假性失败三轮）。返回 result 数组。
//
// ⚠ `params` 的类型**按 api 而定**（2026-11-17 从参考实现 core/device.py:1289 确认）：
//
//	prop/get、prop/set → 数组 [ {...}, {...} ]
//	action             → **单个对象** { ... }     ← 我原来一律传数组，云端回
//	                                               `code=-8 data type not valid`
//
// 参考实现里 `async_do_action(pms)` 的 pms 就是一个 dict，`async_get_props(pms)`
// 的 pms 才是 list。两种形状不能混。
func cloudRequestMIoTSpec(ctx *plugin.Context, sessionID, ssecurity, deviceID, api string,
	params any) ([]map[string]any, error) {
	body, err := json.Marshal(map[string]any{"params": params})
	if err != nil {
		return nil, err
	}
	/*
	 * ★★ 2026-11-19（用户「设备不能控制」真机日志：`switch turn_on result=error
	 *   latency=1124263ms`）：这条云请求**没有任何超时** —— 设备/云端不响应时
	 *   调用会一直挂着，平台只能等自己那侧超时（十几分钟），用户看到的是"点了没反应"。
	 *
	 *   这里在**插件内**加有界等待（标准做法，不动平台代码）：
	 *   超时立刻返回明确错误，界面马上拿到"无响应/离线"而不是无限转圈。
	 */
	const specWriteTimeout = 12 * time.Second
	type specResp struct {
		raw []byte
		err error
	}
	done := make(chan specResp, 1)
	go func() {
		r, e := apiPostRC4GuestDevice(ctx, sessionID, ssecurity, deviceID,
			"https://api.io.mi.com/app/miotspec/"+api, string(body))
		done <- specResp{raw: r, err: e}
	}()
	var raw []byte
	select {
	case v := <-done:
		raw, err = v.raw, v.err
		if err != nil {
			return nil, err
		}
	case <-time.After(specWriteTimeout):
		return nil, fmt.Errorf("设备无响应：%.0f 秒内未返回（可先确认设备在线/同一局域网）", specWriteTimeout.Seconds())
	}
	/*
	 * `result` 的形状**按 api 而不同**（2026-11-17 实测）：
	 *   prop/get → 数组 [ {...} ]
	 *   action   → 单个对象（甚至可能为空）⇒ 之前按数组解会报"响应解析失败"，
	 *              而请求其实已经被接受了（错误码从 -8 变成了这一条）。
	 *
	 * 所以先按 RawMessage 收，再尽力解析：数组 → 直接用；对象 → 包成单元素；
	 * 其它/空 → 返回空切片（调用方对 action 不依赖返回值）。
	 */
	var resp struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("云端响应解析失败：%s", clipForLog(string(raw), 200))
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("云端返回 code=%d message=%s", resp.Code, resp.Message)
	}
	trimmed := strings.TrimSpace(string(resp.Result))
	if trimmed == "" || trimmed == "null" || trimmed == `""` {
		return nil, nil
	}
	var list []map[string]any
	if err := json.Unmarshal(resp.Result, &list); err == nil {
		return list, nil
	}
	var single map[string]any
	if err := json.Unmarshal(resp.Result, &single); err == nil {
		return []map[string]any{single}, nil
	}
	return nil, nil
}

/*
 * 红外遥控：**动作**而不是属性（2026-11-17 按规格核对）。
 *
 * `miir.aircondition.ir02` 的 service iid=2 里：
 *
 *	action iid=5  urn:miot-spec-v2:action:turn-off:…
 *	action iid=6  urn:miot-spec-v2:action:turn-on:…
 *	prop   iid=1  ir-mode (uint8)
 *	prop   iid=2  ir-temperature (float)
 *
 * 也就是说开关是 `miotspec/action`（带 aiid），**不是** `prop/set`。
 * 参考实现的 `button_actions: turn_on,turn_off` 也印证了这一点。
 *
 * 为什么按名字找而不是写死 aiid：不同红外型号（ir01/ir02/xiaomi.ir.*）的
 * 动作编号不同，写死必与实机漂移 —— 温度属性那次已经踩过。
 */
func cloudIRPowerAction(ctx *plugin.Context, model string, on bool) (int, int, bool) {
	typ, ok := lookupModelType(model)
	if !ok {
		return 0, 0, false
	}
	sp, err := loadMIoTSpec(ctx, typ)
	if err != nil {
		return 0, 0, false
	}
	want := ":turn-off:"
	if on {
		want = ":turn-on:"
	}
	for _, svc := range sp.Services {
		for _, a := range svc.Actions {
			if strings.Contains(strings.ToLower(a.Type), want) {
				return svc.IID, a.IID, true
			}
		}
	}
	return 0, 0, false
}

// cloudIRProp 在该型号规格里按属性名后缀找 (siid, piid)。
//
// 红外空调的 `ir-mode`(uint8) / `ir-temperature`(float) 都在 service 2，
// 但不同型号服务编号可能不同，所以按 urn 里的名字找（`:ir-temperature:`）。
func cloudIRProp(ctx *plugin.Context, model, nameSuffix string) (int, int, bool) {
	typ, ok := lookupModelType(model)
	if !ok {
		return 0, 0, false
	}
	sp, err := loadMIoTSpec(ctx, typ)
	if err != nil {
		return 0, 0, false
	}
	for _, svc := range sp.Services {
		for _, pr := range svc.Properties {
			if strings.Contains(strings.ToLower(pr.Type), nameSuffix) {
				return svc.IID, pr.IID, true
			}
		}
	}
	return 0, 0, false
}

// cloudIRSetProp 经云端写一个红外属性（温度/模式）。
func cloudIRSetProp(ctx *plugin.Context, d miioDevice, nameSuffix string, value any) error {
	sessionID, ssecurity, deviceID, err := openCloudSession(ctx)
	if err != nil {
		return fmt.Errorf("无云会话（需先登录）：%w", err)
	}
	// 会话必须关：每次云端调用都新开一条，不关会把 http 会话配额（4 条）打满。
	defer func() { _ = ctx.HTTPClose(sessionID) }()
	siid, piid, ok := cloudIRProp(ctx, d.Model, nameSuffix)
	if !ok {
		return fmt.Errorf("型号 %s 的规格里没有 %s 属性", d.Model, nameSuffix)
	}
	rows, err := cloudRequestMIoTSpec(ctx, sessionID, ssecurity, deviceID, "prop/set",
		[]map[string]any{{"did": d.ExternalID, "siid": siid, "piid": piid, "value": value}})
	if err != nil {
		return err
	}
	for _, r := range rows {
		if code, ok := r["code"].(float64); ok && int(code) != 0 {
			return fmt.Errorf("设备拒绝写入（code=%d siid=%d piid=%d）", int(code), siid, piid)
		}
	}
	return nil
}

// irModeValue 把平台的模式名翻成该型号的枚举值（0=Auto 1=Cool 2=Dry 3=Heat 4=Fan）。
//
// 枚举来自规格的 value-list（miir.aircondition.ir02 实测），不是猜的。
func irModeValue(mode string) (float64, bool) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "auto":
		return 0, true
	case "cool":
		return 1, true
	case "dry":
		return 2, true
	case "heat":
		return 3, true
	case "fan_only", "fan":
		return 4, true
	}
	return 0, false
}

// cloudExecuteIRPower 经云端发一次红外开关动作。
//
// 红外是"发出去就不管"的语义：设备不回执、也没有可回读的开关属性
// （规格里只有 ir-mode / ir-temperature）。所以这里**不伪造状态上报** ——
// 上报"我发了 turn_on"当成"设备已开"正是之前踩过的乐观确认。
func cloudExecuteIRPower(ctx *plugin.Context, d miioDevice, on bool) error {
	sessionID, ssecurity, deviceID, err := openCloudSession(ctx)
	if err != nil {
		return fmt.Errorf("无云会话（需先登录）：%w", err)
	}
	// 会话必须关：每次云端调用都新开一条，不关会把 http 会话配额（4 条）打满。
	defer func() { _ = ctx.HTTPClose(sessionID) }()
	siid, aiid, ok := cloudIRPowerAction(ctx, d.Model, on)
	if !ok {
		return fmt.Errorf("型号 %s 的规格里找不到 turn-on/turn-off 动作（无法经云端控制）", d.Model)
	}
	/*
	 * 形状对齐参考实现 core/device.py:1289：
	 *
	 *	pms = {'did': str(did), 'siid': siid, 'aiid': aiid, 'in': params or []}
	 *	result = await cloud.async_do_action(pms)   # → {'params': pms}（**对象**）
	 *
	 * 即：`in` 要带（空数组），而 `params` 本身必须是**对象**而不是数组。
	 * 之前两种错法（数组 + 有/无 in）都会得到 `code=-8 data type not valid`。
	 */
	rows, err := cloudRequestMIoTSpec(ctx, sessionID, ssecurity, deviceID, "action",
		map[string]any{"did": d.ExternalID, "siid": siid, "aiid": aiid, "in": []any{}})
	if err != nil {
		return err
	}
	for _, r := range rows {
		if code, ok := r["code"].(float64); ok && int(code) != 0 {
			return fmt.Errorf("设备拒绝了红外动作（code=%d siid=%d aiid=%d）", int(code), siid, aiid)
		}
	}
	return nil
}

// clipForLog 截断一段文本用于日志（避免把整包响应灌进日志）。
func clipForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// hasCap 判断能力列表里是否含某个能力名。
func hasCap(caps []string, want string) bool {
	for _, c := range caps {
		if strings.EqualFold(c, want) {
			return true
		}
	}
	return false
}

// mediaAcknowledgedVolume 从一条成功的 volume_set 命令里取出"设备已接受的绝对值"。
//
// 只对 volume_set 成立：`prop/set` 返回 code=0 表示设备**接受**了这个确切的值，
// 上报它不算乐观确认。相比之下 `action turn-on` 回 code=0 只说明"指令发出去了"，
// 结果状态未知 —— 那种一律不上报。
func mediaAcknowledgedVolume(cmd miioCommand) (float64, bool) {
	if !strings.EqualFold(cmd.Action, "volume_set") {
		return 0, false
	}
	raw, ok := cmd.Params["volume_level"]
	if !ok {
		raw = cmd.Params["level"]
	}
	lv, tok := cloudNumber(raw)
	if !tok {
		return 0, false
	}
	if lv < 0 {
		lv = 0
	}
	if lv > 1 {
		lv = 1
	}
	return lv, true
}

// cloudActionByName 在型号规格里按动作名后缀找 (siid, aiid)，并**经云端**执行。
func cloudActionByName(ctx *plugin.Context, d miioDevice, nameSuffix string) error {
	sessionID, ssecurity, deviceID, err := openCloudSession(ctx)
	if err != nil {
		return fmt.Errorf("无云会话（需先登录）：%w", err)
	}
	// 会话必须关：每次云端调用都新开一条，不关会把 http 会话配额（4 条）打满。
	defer func() { _ = ctx.HTTPClose(sessionID) }()
	typ, ok := lookupModelType(d.Model)
	if !ok {
		return fmt.Errorf("型号 %s 不在目录中", d.Model)
	}
	sp, err := loadMIoTSpec(ctx, typ)
	if err != nil {
		return fmt.Errorf("读取规格失败：%w", err)
	}
	for _, svc := range sp.Services {
		for _, a := range svc.Actions {
			if strings.Contains(strings.ToLower(a.Type), nameSuffix) {
				rows, aerr := cloudRequestMIoTSpec(ctx, sessionID, ssecurity, deviceID, "action",
					map[string]any{"did": d.ExternalID, "siid": svc.IID, "aiid": a.IID, "in": []any{}})
				if aerr != nil {
					return aerr
				}
				for _, r := range rows {
					if code, ok := r["code"].(float64); ok && int(code) != 0 {
						return fmt.Errorf("设备拒绝动作（code=%d）", int(code))
					}
				}
				return nil
			}
		}
	}
	return fmt.Errorf("型号 %s 的规格里没有 %s 动作", d.Model, nameSuffix)
}

// cloudMediaCommand 把平台的 media_player 动作翻成云端调用。
//
// 映射：media_play→play、media_pause→pause、media_next→next、media_prev→previous、
// turn_on→wake-up（音箱没有"通电"，唤醒是语义上最接近的"开始工作"）、
// volume_set 写 volume(0~1→0~100)。
//
// 明确拒绝 stop / turn_off：规格里没有 standby/stop，硬凑成 pause 会让人以为真停了。
func cloudMediaCommand(ctx *plugin.Context, d miioDevice, cmd miioCommand) error {
	switch strings.ToLower(cmd.Action) {
	case "media_play", "play":
		return cloudActionByName(ctx, d, ":play:")
	case "media_pause", "pause":
		return cloudActionByName(ctx, d, ":pause:")
	case "media_next", "next":
		return cloudActionByName(ctx, d, ":next:")
	case "media_prev", "previous":
		return cloudActionByName(ctx, d, ":previous:")
	case "turn_on":
		return cloudActionByName(ctx, d, ":wake-up:")
	case "volume_set":
		raw, ok := cmd.Params["volume_level"]
		if !ok {
			raw = cmd.Params["level"]
		}
		lv, tok := cloudNumber(raw)
		if !tok {
			return fmt.Errorf("音量参数不可用（收到 %v）", raw)
		}
		if lv < 0 {
			lv = 0
		}
		if lv > 1 {
			lv = 1
		}
		return cloudIRSetProp(ctx, d, ":volume:", int(lv*100+0.5))
	case "turn_off", "media_stop":
		return fmt.Errorf("音箱不支持 %s（规格里没有 standby/stop；停播放请用暂停）", cmd.Action)
	}
	return fmt.Errorf("不支持的媒体动作 %q", cmd.Action)
}

/*
 * lanMediaState 用**局域网老方法**读音箱音量/静音（2026-11-17 实测可行）。
 *
 * 探测结论（`lan_diagnose`，小爱音箱Pro lx06）：
 *   `miIO.info`   ✅      `get_prop`     ✅（属性名要用 volume/mute）
 *   `get_properties`（MIoT）✗   `player_play_operation` ✗
 *   各种 `ir_play` 变体 ✗
 *
 * 所以**状态读取可以完全离线**（不依赖云会话），而播放控制仍只能云端。
 * 这条优先于云端：云一过期，至少音量/静音还能看。
 */
func lanMediaState(ctx *plugin.Context, d miioDevice, token []byte) (map[string]any, error) {
	env, err := miioExchange(ctx, d, token, "get_prop", []string{"volume", "mute"})
	if err != nil {
		return nil, err
	}
	list, ok := env["result"].([]any)
	if !ok || len(list) == 0 {
		return nil, errors.New("get_prop 返回不是数组")
	}
	mp := map[string]any{}
	if v, ok := cloudNumber(list[0]); ok {
		mp["volume_level"] = v / 100.0
	}
	if len(list) > 1 {
		if s, ok := list[1].(string); ok {
			mp["muted"] = strings.EqualFold(s, "on") || strings.EqualFold(s, "true")
		} else if b, ok := list[1].(bool); ok {
			mp["muted"] = b
		}
	}
	if len(mp) == 0 {
		return nil, errors.New("get_prop 没有可用读数")
	}
	return map[string]any{"media_player": mp}, nil
}

// cloudMediaState 经云端读音箱音量/静音/播放状态，组装成平台认的 media_player 键。
func cloudMediaState(ctx *plugin.Context, d miioDevice) (map[string]any, error) {
	sessionID, ssecurity, deviceID, err := openCloudSession(ctx)
	if err != nil {
		return nil, err
	}
	// 会话必须关：每次云端调用都新开一条，不关会把 http 会话配额（4 条）打满。
	defer func() { _ = ctx.HTTPClose(sessionID) }()
	type ref struct{ key string }
	refs := map[string]ref{}
	var params []map[string]any
	typ, ok := lookupModelType(d.Model)
	if !ok {
		return nil, fmt.Errorf("型号 %s 不在目录中", d.Model)
	}
	sp, err := loadMIoTSpec(ctx, typ)
	if err != nil {
		return nil, err
	}
	for _, svc := range sp.Services {
		for _, pr := range svc.Properties {
			low := strings.ToLower(pr.Type)
			var key string
			switch {
			case strings.Contains(low, ":volume:"):
				key = "volume"
			case strings.Contains(low, ":mute:"):
				key = "mute"
			case strings.Contains(low, ":playing-state:"):
				key = "playing"
			default:
				continue
			}
			params = append(params, map[string]any{"did": d.ExternalID, "siid": svc.IID, "piid": pr.IID})
			refs[fmt.Sprintf("%d.%d", svc.IID, pr.IID)] = ref{key: key}
		}
	}
	if len(params) == 0 {
		return nil, errors.New("规格里没有音量/静音/播放状态属性")
	}
	rows, err := cloudRequestMIoTSpec(ctx, sessionID, ssecurity, deviceID, "prop/get", params)
	if err != nil {
		return nil, err
	}
	mp := map[string]any{}
	for _, r := range rows {
		if code, ok := r["code"].(float64); ok && int(code) != 0 {
			continue
		}
		siid, _ := r["siid"].(float64)
		piid, _ := r["piid"].(float64)
		rf, ok := refs[fmt.Sprintf("%d.%d", int(siid), int(piid))]
		if !ok {
			continue
		}
		switch rf.key {
		case "volume":
			if v, ok := r["value"].(float64); ok {
				mp["volume_level"] = v / 100.0
			}
		case "mute":
			if v, ok := r["value"].(bool); ok {
				mp["muted"] = v
			}
		case "playing":
			if v, ok := r["value"].(float64); ok {
				if int(v) == 1 {
					mp["state"] = "playing"
				} else {
					mp["state"] = "idle"
				}
			}
		}
	}
	if len(mp) == 0 {
		return nil, errors.New("云端没有返回媒体状态")
	}
	return map[string]any{"media_player": mp}, nil
}

// hasSpecDrivenCap 判断这条命令是不是"规格驱动"的能力（button / number / 非主开关的 switch 实例）。
//
// 判据：能力名是 button/number，或 switch 且 instance_id 不是空/switch/number 之类主实例。
func hasSpecDrivenCap(caps []string, cmd miioCommand) bool {
	cap := strings.ToLower(cmd.Capability)
	if cap == "button" || cap == "number" {
		return true
	}
	/*
	 * ★ 2026-11-18：**非主开关的 switch 实例**（指示灯 / 麦克风静音 / 睡眠模式…）
	 * 也必须走规格驱动，否则控制会打错地方。
	 *
	 * 事故（用户：「同一个能力多个开关有问题，插件侧测试控制 OK，页面用不了」）：
	 * 台灯的规格里有两个 bool 属性 —— service 2 的主开关 `on`、
	 * service 3 的指示灯 `indicator_light_on`。插件把它们投影成两个 switch 实例
	 * （`switch` / `indicator_light_on`），平台各画一个开关。
	 * 但 `mapControl` 的 turn_on/turn_off 是**硬编码 siid 2 / piid 1** 的，
	 * 于是指示灯那颗开关实际去控制主灯 —— 表现就是「点这个开关，亮的却是另一个」。
	 *
	 * 判据：主开关的实例名与能力同名（投影时特意还原成 `switch`，为的是和
	 * 状态行的 instance_id 对齐），其余实例名形如 `服务名_属性名`。
	 * 主开关继续走 set_power 快路径（快且不需要读规格），其余走规格写入。
	 */
	if cap == "switch" {
		inst := strings.ToLower(strings.TrimSpace(commandInstanceID(cmd)))
		return inst != "" && inst != cap
	}
	return false
}

// execSpecDriven 把一条"规格驱动"的命令落到云端 action / prop/set。
//
// 定位规则（都按**规格里的事实**找，不写死 iid）：
//   · action 名从 cmd.Action 或 instance_id 推：`press` → 用 instance 指定的动作名；
//   · number 的 set_value → 用 instance 指定的属性名；
//   · switch 的 turn_on/off → 用 instance 指定的 bool 属性。

// specNameMatches 判断实例名是否指向规格里的某个名字。
//
// 实例名是 `服务名_名字`（`intelligent_speaker_play_radio`、`clock_stop_alarm`、
// `speaker_volume`），规格里的名字只是最后那段（`play-radio` / `stop-alarm` / `volume`）。
// 用**后缀匹配**（把下划线归一成连字符后比较）而不是切最后一个下划线 ——
// 动作名本身可能带连字符（`stop-alarm`），切会切错。
func specNameMatches(instName, specName string) bool {
	a := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(instName)), "_", "-")
	b := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(specName)), "_", "-")
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasSuffix(a, "-"+b)
}

func execSpecDriven(ctx *plugin.Context, d miioDevice, cmd miioCommand) error {
	inst := commandInstanceID(cmd)
	sessionID, ssecurity, deviceID, err := openCloudSession(ctx)
	if err != nil {
		return fmt.Errorf("无云会话（需先登录）：%w", err)
	}
	defer func() { _ = ctx.HTTPClose(sessionID) }()

	typ, ok := lookupModelType(d.Model)
	if !ok {
		return fmt.Errorf("型号 %s 不在目录中", d.Model)
	}
	sp, err := loadMIoTSpec(ctx, typ)
	if err != nil {
		return fmt.Errorf("读取规格失败：%w", err)
	}

	switch strings.ToLower(cmd.Capability) {
	case "button":
		/*
		 * press：动作名来自 instance_id。
		 *
		 * ⚠ instance_id 是 `服务名_动作名`（如 `intelligent_speaker_play_radio`），
		 * 用于平台侧唯一键；**动作名本身只是最后那段**（`play-radio`）。
		 * 直接拿实例名去比动作名会永远匹配不到（实测报
		 * 「规格里找不到动作 intelligent_speaker_play_radio」）。
		 */
		want := inst
		if want == "" || strings.EqualFold(want, "press") {
			return fmt.Errorf("缺少动作名（instance_id）")
		}
		for _, svc := range sp.Services {
			for _, a := range svc.Actions {
				name := actionNameOf(a.Type)
				if specNameMatches(want, name) {
					rows, aerr := cloudRequestMIoTSpec(ctx, sessionID, ssecurity, deviceID, "action",
						map[string]any{"did": d.ExternalID, "siid": svc.IID, "aiid": a.IID, "in": []any{}})
					if aerr != nil {
						return aerr
					}
					for _, r := range rows {
						if code, ok := r["code"].(float64); ok && int(code) != 0 {
							return fmt.Errorf("设备拒绝动作 %s（code=%d）", want, int(code))
						}
					}
					return nil
				}
			}
		}
		return fmt.Errorf("规格里找不到动作 %q", inst)
	case "number":
		raw, ok := cmd.Params["value"]
		if !ok {
			raw = cmd.Params["number"]
		}
		v, tok := cloudNumber(raw)
		if !tok {
			return fmt.Errorf("数值参数不可用（收到 %v）", raw)
		}
		want := inst
		for _, svc := range sp.Services {
			for _, pr := range svc.Properties {
				if !specNameMatches(want, actionNameOf(pr.Type)) {
					continue
				}
				rows, perr := cloudRequestMIoTSpec(ctx, sessionID, ssecurity, deviceID, "prop/set",
					[]map[string]any{{"did": d.ExternalID, "siid": svc.IID, "piid": pr.IID, "value": v}})
				if perr != nil {
					return perr
				}
				for _, r := range rows {
					if code, ok := r["code"].(float64); ok && int(code) != 0 {
						return fmt.Errorf("设备拒绝写入 %s（code=%d）", want, int(code))
					}
				}
				return nil
			}
		}
		return fmt.Errorf("规格里找不到可选属性 %q", inst)
	case "switch":
		// 非主开关的 switch 实例（如麦克风静音 / 睡眠模式）：写该 bool 属性。
		want := inst
		if want == "" {
			return fmt.Errorf("缺少属性名（instance_id）")
		}
		on := strings.EqualFold(cmd.Action, "turn_on")
		for _, svc := range sp.Services {
			for _, pr := range svc.Properties {
				if !specNameMatches(want, actionNameOf(pr.Type)) || pr.Format != "bool" {
					continue
				}
				rows, perr := cloudRequestMIoTSpec(ctx, sessionID, ssecurity, deviceID, "prop/set",
					[]map[string]any{{"did": d.ExternalID, "siid": svc.IID, "piid": pr.IID, "value": on}})
				if perr != nil {
					return perr
				}
				for _, r := range rows {
					if code, ok := r["code"].(float64); ok && int(code) != 0 {
						return fmt.Errorf("设备拒绝写入 %s（code=%d）", want, int(code))
					}
				}
				return nil
			}
		}
		return fmt.Errorf("规格里找不到 bool 属性 %q", inst)
	}
	return fmt.Errorf("不支持的规格驱动能力 %q", cmd.Capability)
}

/*
 * tryCloudCommand 尝试用**云端**执行一条命令（`lan_priority=cloud` 时优先走它）。
 *
 * 返回 (handled, err)：
 *   · handled=false → 云端不执行这类命令（如亮度/模式），调用方走局域网
 *   · handled=true, err=nil → 云端已执行
 *   · handled=true, err!=nil → 云端支持但执行失败（调用方可回退局域网）
 *
 * 只覆盖云端确实支持的几类：开关（switch）、红外（已在前面分流）、
 * 媒体控制（已有 cloudMediaCommand）。其余动作返回 handled=false 是**诚实**的
 * —— 云端不支持就不假装支持。
 */
func tryCloudCommand(ctx *plugin.Context, d miioDevice, cmd miioCommand) (bool, error) {
	switch strings.ToLower(cmd.Action) {
	case "turn_on", "turn_off":
		if !hasCap(d.Capabilities, "switch") {
			return false, nil
		}
		on := strings.EqualFold(cmd.Action, "turn_on")
		if err := cloudSetPower(ctx, d, on); err != nil {
			return true, err
		}
		// 不伪造状态：云端 set 成功只代表"请求被接受"，
		// 真实开关态由周期轮询读回（与红外同一口径）。
		return true, nil
	}
	return false, nil
}

// cloudSetPower 经云端设置开关（`lan_priority=cloud` 时优先走它）。
//
// 实现走 MIoT `prop/set`：从规格里找**可写 bool 且名字像开关**的属性。
// 不写死 siid/piid —— 不同型号位置不同，写死就是硬编码。
func cloudSetPower(ctx *plugin.Context, d miioDevice, on bool) error {
	sessionID, ssecurity, deviceID, err := openCloudSession(ctx)
	if err != nil {
		return fmt.Errorf("无云会话（需先登录）：%w", err)
	}
	defer func() { _ = ctx.HTTPClose(sessionID) }()

	typ, ok := lookupModelType(d.Model)
	if !ok {
		return fmt.Errorf("型号 %s 不在目录中", d.Model)
	}
	sp, serr := loadMIoTSpec(ctx, typ)
	if serr != nil {
		return fmt.Errorf("读取规格失败：%w", serr)
	}
	// 候选属性名：on / power / switch-status（按常见顺序）
	cands := []string{"on", "power", "switch-status", "switch_status"}
	for _, svc := range sp.Services {
		for _, pr := range svc.Properties {
			if !hasAccess(pr.Access, "write") || pr.Format != "bool" {
				continue
			}
			name := actionNameOf(pr.Type)
			match := false
			for _, c := range cands {
				if name == c {
					match = true
					break
				}
			}
			if !match {
				continue
			}
			rows, perr := cloudRequestMIoTSpec(ctx, sessionID, ssecurity, deviceID, "prop/set",
				[]map[string]any{{"did": d.ExternalID, "siid": svc.IID, "piid": pr.IID, "value": on}})
			if perr != nil {
				return perr
			}
			for _, r := range rows {
				if code, ok := r["code"].(float64); ok && int(code) != 0 {
					return fmt.Errorf("云端拒绝设置开关（code=%d）", int(code))
				}
			}
			return nil
		}
	}
	return fmt.Errorf("型号 %s 的规格里没有可写的开关属性", d.Model)
}
