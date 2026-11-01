// HX-MI ABI v2 加载式插件入口。
//
// 网络、文件、调度与设备注册全部通过宿主资源面；本模块不监听 HTTP/MQTT 端口，
// 不启动 ticker/守护进程。云登录和 miIO 编解码会在后续等价迁移中复用此适配层。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"homex.dev/homex/sdk/go/plugin"
)

type config struct {
	Region          string `json:"region"`
	SyncIntervalSec int    `json:"sync_interval_seconds"`
	// PollingEnabled 控制后台自动同步；缺省视为开启（旧配置不受影响）。
	PollingEnabled *bool `json:"polling_enabled,omitempty"`
	// DeviceFilterModels 非空时只同步 model 前缀匹配的设备（空 = 全部）。
	DeviceFilterModels []string `json:"device_filter_models,omitempty"`
}

var current = config{Region: "cn", SyncIntervalSec: 300}

func pollingEnabled() bool {
	return current.PollingEnabled == nil || *current.PollingEnabled
}

func init() {
	plugin.Register(plugin.Plugin{Configure: configure, Call: call, Health: health, Event: event})
}

func main() {}

func configure(ctx *plugin.Context, raw json.RawMessage) error {
	var next config
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &next); err != nil {
			return fmt.Errorf("配置解析失败: %w", err)
		}
	}
	if next.Region == "" {
		next.Region = "cn"
	}
	if next.SyncIntervalSec == 0 {
		next.SyncIntervalSec = 300
	}
	if next.SyncIntervalSec < 60 || next.SyncIntervalSec > 86400 {
		return errors.New("sync_interval_seconds 必须在 60 到 86400 之间")
	}
	for _, f := range next.DeviceFilterModels {
		if strings.TrimSpace(f) == "" {
			return errors.New("device_filter_models 不能包含空字符串")
		}
	}
	current = next
	if err := migrateLegacy(ctx); err != nil {
		return fmt.Errorf("旧数据迁移失败: %w", err)
	}
	if _, err := ctx.UDPListen(54321); err != nil {
		return fmt.Errorf("active udp listener: %w", err)
	}
	if pollingEnabled() {
		if err := ctx.BackgroundRegister(plugin.BackgroundRegisterRequest{Name: "account-sync", IntervalMS: next.SyncIntervalSec * 1000, Immediate: true}); err != nil {
			return err
		}
	}
	// MIoT 目录每月刷新一次；由宿主统一调度（jitter/失败退避/disable 清理在
	// 宿主 scheduler），插件自己不起 ticker，也不在 Boot 时阻塞网络。
	// 局域网健康探测：30 秒一轮，只发 hello 包（无令牌、无副作用）。
	// 用户明确要求高频探测，本地探不到就立即标离线，不等云端账号同步。
	if err := ctx.BackgroundRegister(plugin.BackgroundRegisterRequest{
		Name: "lan-health", IntervalMS: int(lanHealthProbeInterval / time.Millisecond), Immediate: true,
	}); err != nil {
		return err
	}
	return ctx.BackgroundRegister(plugin.BackgroundRegisterRequest{Name: "catalog-refresh", IntervalMS: 30 * 24 * 3600 * 1000, Immediate: false})
}

func call(ctx *plugin.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	// 消费"宿主要求自动同步"的标记：在这里做是安全的 ——
	// 我们此刻**就是**那次被串行化的调用，持有实例锁，可以在返回前同步执行，
	// 不存在 tick 里起 goroutine 那种自死锁。
	//
	// 只对**只读**工具顺带同步：login/verify/logout 等会改凭据，混进去会让
	// "我刚点的那个操作"与"后台同步"互相干扰，出了错也难分辨是谁的。
	if tool != "login" && tool != "verify" && tool != "verify_web" &&
		tool != "logout" && tool != "sync_now" && tool != "account_delete" {
		consumePendingAutoSync(ctx)
	}
	switch tool {
	case "regions":
		return json.Marshal(map[string]any{"regions": []map[string]any{{"id": "cn", "name": "中国大陆", "enabled": true}}})
	case "accounts":
		store, err := openAccountStore(ctx)
		if err != nil {
			return nil, err
		}
		items, err := store.list()
		if err != nil {
			return nil, err
		}
		// 顶层再给一份完整账号映射：用户明确要求账号显示完整（不要 *）。
		// 放在独立字段而不是塞进 items 里，是为了不改变既有 wire 契约 ——
		// items 的 masked_account 仍供需要脱敏的调用方使用。
		full := make(map[string]string, len(items))
		if e, lerr := store.load(); lerr == nil {
			for _, a := range e.Accounts {
				if a.CloudIdentity != "" {
					full[a.AccountID] = a.CloudIdentity
				}
			}
		}
		return json.Marshal(map[string]any{"accounts": items, "account_names": full})
	case "account_settings":
		var in struct {
			AccountID       string  `json:"account_id"`
			Revision        uint64  `json:"revision"`
			HouseholdID     *string `json:"household_id"`
			SyncEnabled     *bool   `json:"sync_enabled"`
			IntervalSeconds *int    `json:"interval_seconds"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, err
		}
		store, err := openAccountStore(ctx)
		if err != nil {
			return nil, err
		}
		a, err := store.update(in.AccountID, in.Revision, in.HouseholdID, in.SyncEnabled, in.IntervalSeconds)
		if err != nil {
			return nil, err
		}
		return json.Marshal(a)
	case "account_sync":
		// ## 之前这里是空壳：return {"state":"queued"} 什么都不做
		//
		// 前端点"手动同步"会调它，拿到 queued 就当成功，然后回读状态发现
		// 什么都没变 —— 用户看到的是"同步了但设备列表不动"。
		// 现在真的跑一次同步，并把结果如实返回（成功/失败都要能区分）。
		//
		// 限制（必须说清，不能假装支持）：当前宿主只维持**一个**云端会话，
		// 因此这里同步的是"当前登录的那个账号"。多账号并发同步需要每个账号
		// 各自的可恢复会话，尚未实现。
		summary, serr := syncCloudDevices(ctx)
		if serr != nil {
			return json.Marshal(map[string]any{"state": "failed", "error": serr.Error()})
		}
		return json.Marshal(map[string]any{"state": "succeeded", "summary": summary})
	case "account_delete":
		var in struct {
			AccountID string `json:"account_id"`
			Revision  uint64 `json:"revision"`
			Confirm   bool   `json:"confirm"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, err
		}
		if !in.Confirm {
			return nil, errors.New("删除账号需要 confirm=true")
		}
		store, err := openAccountStore(ctx)
		if err != nil {
			return nil, err
		}
		if err = store.delete(in.AccountID, in.Revision); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"state": "deleted", "account_id": in.AccountID})
	case "sync_now":
		if _, err := syncCloudDevices(ctx); err != nil {
			return json.Marshal(map[string]any{"started": false, "reason": err.Error()})
		}
		return json.Marshal(map[string]any{"started": true})
	case "login":
		return startLogin(ctx, args)
	case "status":
		return loginStatus(ctx, args)
	case "logout":
		return logoutLogin(ctx, args)
	case "captcha":
		return captchaTool(ctx, args)
	case "captcha_image":
		return captchaImageTool(ctx, args)
	case "verify":
		// 短信/邮件验证码与网页落地链接是**两条不同协议**，不能共用分支：
		// 共用的后果是用户输入 6 位验证码后，代码去读 url（空串）并报
		// “链接为空”，把“功能未实现”伪装成“用户没贴链接”。
		// 目前插件侧只实测通网页落地链接；验证码渠道如实返回 unsupported。
		var in struct {
			SessionID string `json:"session_id"`
			Code      string `json:"code"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, err
		}
		if strings.TrimSpace(in.Code) == "" {
			return nil, errors.New("验证码为空")
		}
		s, err := loadLoginSession(ctx, in.SessionID)
		if err != nil {
			return nil, err
		}
		return verifyByCode(ctx, in.SessionID, &s, in.Code)
	case "verify_web":
		var in struct {
			SessionID string `json:"session_id"`
			URL       string `json:"url"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, err
		}
		s, err := loadLoginSession(ctx, in.SessionID)
		if err != nil {
			return nil, err
		}
		loc, why := normalizeTrustedSTS(in.URL)
		if loc == "" {
			return nil, fmt.Errorf("验证落地链接不可信：%s", why)
		}
		// 先在宿主 jar 里吸收粘贴的落地链接（浏览器完成验证后 serviceToken
		// 只存在于该链接），再用 step1 取**配对**的 (ssecurity, location) 并
		// GET 该 location——否则签名钥匙与票据错配，小米报 invalid signature。
		if err := absorbLocation(ctx, in.SessionID, s.DeviceID, loc); err != nil {
			return nil, err
		}
		ssecurity, passToken, serr := refreshPairedMaterial(ctx, in.SessionID, s.DeviceID)
		if serr != nil {
			return nil, serr
		}
		if ssecurity == "" {
			return nil, errors.New("验证已通过，但小米未返回会话密钥；请重新登录再试")
		}
		out, perr := exchangeAndProbe(ctx, in.SessionID, &s, "", ssecurity, passToken)
		// 同 verifyByCode：完成后关闭会话并清理文件，避免耗尽宿主会话额度。
		_ = ctx.HTTPClose(in.SessionID)
		_ = ctx.FSRemove(sessionPath(in.SessionID))
		challengeSecrets.delete(in.SessionID)
		return out, perr
	case "account":
		return buildAccountView(ctx)
	case "settings":
		return applySyncSettingsTool(ctx, args)
	case "lan_health":
		return lanHealthTool(ctx)
	case "catalog_status":
		return catalogStatusTool(ctx)
	case "catalog_refresh":
		return catalogRefreshTool(ctx)
	default:
		return nil, fmt.Errorf("未知工具 %q", tool)
	}
}

func event(ctx *plugin.Context, subject string, payload json.RawMessage) error {
	// 事件入口留一条诊断：后台任务"派发了却不执行"时，这是判断
	// "事件没送到插件"与"送到了但分支没匹配上"的唯一分界。
	ctx.Log(0, "hx-mi event: "+subject)
	switch {
	case strings.HasSuffix(subject, ".background.task.tick"):
		var tick struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(payload, &tick)
		if tick.Name == "catalog-refresh" {
			idx, source, err := refreshModelIndex(ctx, false)
			if err != nil {
				ctx.Log(2, "hx-mi catalog refresh failed: "+err.Error())
				return nil
			}
			_ = writeCoverageReport(ctx, coverageForIndex(ctx, idx))
			ctx.Log(0, "hx-mi catalog refresh: "+source)
			return nil
		}
		if tick.Name == "lan-health" {
			// 同样不在 tick 里做重活：probeAllLanDevices 会对每台设备发 UDP
			// 并等最多 2 秒，设备多时累计可达十几秒，足以让宿主调度循环卡住。
			// 这里只置位，由外部按需取用（lan_health 工具仍可立即探测）。
			lanProbePending.Store(true)
			ctx.Log(1, "hx-mi lan-health tick: marked pending")
			return nil
		}
		if tick.Name == "account-sync" {
			// ## tick 里不**同步**跑云同步，但必须真的把它跑起来
			//
			// 两个约束同时成立：
			//   ① tick 不能阻塞 —— 一次慢请求会拖死宿主 scheduler.loop，
			//      让所有周期任务停摆（已实测）；
			//   ② 自动同步必须真的发生 —— 只置位标记而没人消费，等于没实现。
			//
			// 折中：在插件自己的 goroutine 里异步跑，tick 立刻返回。
			// 用 CAS 保证同一时刻只有一次同步在跑，避免慢请求把 goroutine 堆起来。
			// ## 不能在 tick 里起 goroutine 做 host_call（会自死锁）
			//
			// 宿主 CallOp 对单个 wasm 实例是**串行**的（in.mu）：tick 正在被调用时
			// 锁被持有。若在 tick 里 `go runAsyncSync(ctx)`，那个 goroutine 立刻
			// 就要发起 host_call（FSRead / HTTP…），同样需要这把锁 —— 而 tick 还没
			// 返回，锁不会释放 ⇒ 双向等待、永久死锁。
			//
			// 实测特征：03:59 那次之后每轮 tick 都打印 "previous sync still
			// running, skip"，而 sync-state.json 永远不生成 —— 因为那次同步
			// 卡在第一次 host_call 上再也没动过。
			//
			// 正确做法：tick 只置位（纯内存操作，不碰 host），由下一次**正常工具
			// 调用**顺带消费。代价是同步时点依赖用户/平台的其他调用；换来的是
			// 绝不会死锁，而"界面上看到的数据"本身就是调用触发的，这一点可接受。
			cloudSyncPending.Store(true)
			ctx.Log(1, "hx-mi account-sync tick: marked pending (consumed on next tool call)")
			return nil
		}
		ctx.Log(0, "hx-mi background tick: "+tick.Name)
	case strings.HasSuffix(subject, ".driver.command"):
		return handleDriverCommand(ctx, payload)
	case strings.HasSuffix(subject, ".miot.event"):
		return handleMIoTEvent(ctx, payload)
	case strings.HasSuffix(subject, ".udp.datagram"):
		return handleActiveUDP(ctx, payload)
	case strings.HasSuffix(subject, ".mqtt.message"):
		ctx.Log(0, "hx-mi resource event received")
	}
	return nil
}

func health(*plugin.Context) (plugin.HealthStatus, error) {
	return plugin.HealthStatus{OK: true, Message: "ABI v2 资源适配已加载；业务迁移未完成"}, nil
}

var _ = time.Second
