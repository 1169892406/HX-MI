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
	// 旧数据迁移是**可选能力**，失败不得阻断插件启用（2026-11-09 修正）。
	//
	// # 为什么改
	//
	// 原实现把迁移失败当成致命错误：部署没有把 `hx_mi_legacy` 映射到只读目录
	// （平台按 fail-closed 拒绝 —— 这是正确的安全设计，源必须是部署显式给出的
	// 白名单）时，整个配置下发失败，用户看到的是
	//   PLUGIN_PERMISSION_DENIED：legacy: 源 "hx_mi_legacy" 未在宿主配置中映射
	// 而用户此时的真实诉求是"用插件"，不是"迁移旧数据"——尤其在新装、无旧数据
	// 的部署里，这条硬失败让插件完全不可用，代价和收益完全不成比例。
	//
	// # 改后语义
	//
	//   · 有旧数据且源已映射 ⇒ 正常迁移（行为不变）；
	//   · 无源映射 / 旧数据不可读 ⇒ **跳过并 Warn**，插件照常启用；
	//   · 迁移中途失败 ⇒ 同样只 Warn：migrateLegacy 内部保证"未提交则旧源可恢复"，
	//     不会留下半截数据，所以重试是安全的（下次 Configure 会再试一次）。
	//
	// 这不是掩盖问题：Warn 日志带着完整原因，运维配好 legacy_sources 并重启后
	// 会自动重试成功。
	if err := migrateLegacy(ctx); err != nil {
		ctx.LogWarn("旧数据迁移跳过（插件仍可用）：%v", err)
	}
	if _, err := ctx.UDPListen(54321); err != nil {
		return fmt.Errorf("active udp listener: %w", err)
	}
	if pollingEnabled() {
		if err := ctx.BackgroundRegister(plugin.BackgroundRegisterRequest{Name: "account-sync", IntervalMS: next.SyncIntervalSec * 1000, Immediate: true}); err != nil {
			return err
		}
		/*
		 * ★★★ 2026-11-16（用户「插件定时通过接口推送同步设备数据到平台」）：
		 *
		 * **这一条才是真正干活的**，上面那个 BackgroundRegister 只是"敲门"。
		 *
		 * ## 为什么必须另起一条平台定时任务
		 *
		 * 上面的 `account-sync` 走的是宿主 background tick。tick 回调发生在
		 * 宿主持有该 wasm 实例锁的时候（CallOp 对单实例串行），所以 tick 里
		 * **任何 host_call 都会自死锁**（代码里 319 行那段注释记了 03:59 那次
		 * 实测：sync-state.json 从此再没生成过）。于是 tick 只能
		 * `cloudSyncPending.Store(true)` 置位，等"下一次工具调用"来消费。
		 *
		 * 问题就在这儿：**平台自己从不调插件工具**。实测统计——
		 * account-sync 投递 694 次、真正同步 0 次，日志清一色
		 * `marked pending (consumed on next tool call)`；
		 * 而注册表因此长期为空 ⇒ 平台控制命令全被判「插件设备不存在」。
		 *
		 * `ScheduleCreate` 是平台本来就提供的正路（本插件 manifest 已声明
		 * `schedule.create` 权限，但此前**从未使用**）：平台把到点的任务
		 * 当作**一次普通工具调用**执行（`pluginsvc.runSchedule` → `CallTool`），
		 * 此时没有 tick 的那把实例锁 ⇒ 可以正常发 host_call ⇒ 真正跑完
		 * `sync_now` → `syncCloudDevices` → `ctx.DeviceAnnounce(...)`，
		 * 设备数据就被推到平台了。
		 *
		 * 三个附带好处：
		 *  · **持久化**：`plugin_schedules` 是库表，服务重启后照样到期执行
		 *    （内存 tick 则要等插件重新注册 + 有人来消费）；
		 *  · **幂等**：平台按 `(installation_id, name)` upsert，同名重复注册
		 *    只是更新 spec，不会堆出一串重复任务（见 repo.scheduleInsert）；
		 *  · **可审计**：`last_run_at` / `run_count` / `last_error` 都在表里，
		 *    "到底有没有同步"不再靠猜。
		 *
		 * 失败只 Warn：定时任务是增强能力，不能因此让插件启用失败。
		 */
		if err := registerAutoSyncSchedule(ctx, next.SyncIntervalSec); err != nil {
			ctx.LogWarn("自动同步定时任务注册失败（插件仍可用，手动同步不受影响）：%v", err)
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
	/*
	 * 启用即按**本地登记表**宣布设备（2026-11-17）。
	 *
	 * 设备注册此前只发生在云同步里 ⇒ 云会话过期时平台注册表为空，连局域网
	 * 设备的命令都会被拒（「设备未注册」）。这里先离线宣布一次，
	 * 让"账号过期不影响设备控制"成立；在线状态随后由局域网探测纠正。
	 */
	announceLocalDevices(ctx, nil)
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
	/*
	 * 局域网探测的消费点（2026-11-17）。
	 *
	 * lan-health tick 只置位（避免在 tick 里做 UDP 重活），真正探测在这里做 ——
	 * 此刻持有实例锁，可以安全发 host_call。探测结果会**更新设备可用性**并
	 * 用本地登记表重新宣布，因此"设备在不在局域网"不再依赖云端。
	 *
	 * 对**所有**工具都消费（包括 sync_now/accounts）：这不是改凭据的活，
	 * 而且用户打开配置页时最需要看到最新的在线状态。
	 */
	consumePendingLanProbe(ctx)
	switch tool {
	case "regions":
		return json.Marshal(map[string]any{"regions": []map[string]any{{"id": "cn", "name": "中国大陆", "enabled": true}}})
	case "accounts":
		store, err := openAccountStore(ctx)
		if err != nil {
			/*
			 * ## 全新安装不该报错（2026-11-09 修「账号列表显示失败」）
			 *
			 * `openAccountStore` 在"没有 accounts_master_key 且没有会话材料"时
			 * 返回错误 —— 但那正是**刚装好、还没登录过任何账号**的状态。
			 * 把它当错误抛出去，界面会显示一条红报错，用户以为插件坏了。
			 *
			 * 判据：账号文件根本不存在 ⇒ 空列表（诚实：确实一个都没有）；
			 * 文件存在却打不开 ⇒ 真错误（数据在但读不了，必须说）。
			 */
			if !accountFileExists(ctx) {
				return json.Marshal(map[string]any{"accounts": []any{}, "account_names": map[string]string{}})
			}
			return nil, err
		}
		items, err := store.list()
		if err != nil {
			return nil, err
		}
		/*
		 * ★★★ 2026-11-16（用户实测截图：顶部红字「同步失败：无云会话（需先登录）」，
		 * 而下面账号列表却标绿「就绪」——自相矛盾；用户判定「应该是 token 过期重新登录」）。
		 *
		 * 真因：`account.State` 只在**登录那一刻**按 `HouseholdID != ""` 写成 ready，
		 * 之后**从不复查会话是否还能用**。于是 token 过期/密文丢失后，账号永远显示
		 * "就绪"，用户完全没有提示该去重新登录 —— 而同步一直在失败。
		 *
		 * 判据必须来自**真实可用性**（能不能读出云会话密钥），而不是"曾经登录过"。
		 * 这里只读一次会话（本地文件 + 一次 vault 解封），成本可忽略。
		 */
		if ok, why := cloudSessionUsable(ctx); !ok {
			ctx.LogWarn("hx-mi 云会话不可用（账号将显示需重新登录）：%s", why)
			for i := range items {
				if items[i].State == "ready" || items[i].State == "ready_unbound" {
					items[i].State = "auth_required"
				}
			}
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
		/*
		 * ★★★ 2026-11-16（定时推送排查）：**失败必须作为错误返回**，不能只放进
		 * 返回体里的 `started:false`。
		 *
		 * 为什么：这个工具现在是平台定时任务（hxmi-auto-sync）的目标。平台的
		 * `pluginsvc.runSchedule` 只按 **error** 记录 `last_error`——
		 * 工具返回 `{"started":false,"reason":...}` 而 error 为 nil 时，
		 * 平台会记成"执行成功"。实测就是这么被瞒过去的：
		 * 定时任务 run_count 一路增长、last_error 为空，
		 * 而插件注册表始终没有设备（控制命令全走"注册表为空"的兜底），
		 * 没有任何一处能看出同步其实失败了。
		 *
		 * 现在：先 LogWarn 留一条人话，再把真错误返回给平台 ——
		 * `plugin_schedules.last_error` 会直接写明原因，排查不用再猜。
		 */
		if _, err := syncCloudDevices(ctx); err != nil {
			ctx.LogWarn("hx-mi sync_now 失败：%v", err)
			return nil, fmt.Errorf("设备同步失败：%w", err)
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
	case "lan_diagnose":
		return lanDiagnoseTool(ctx, args)
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


// autoSyncScheduleName 是平台侧定时任务的名字。
//
// 固定名字是有意的：平台 `scheduleInsert` 按 `(installation_id, name)` upsert，
// 同一个名字重复注册只会更新 spec —— 用户改同步间隔时不需要先删旧任务，
// 也不会在 plugin_schedules 里堆出一串历史任务。
const autoSyncScheduleName = "hxmi-auto-sync"

// registerAutoSyncSchedule 让**平台**按用户设定的间隔调用本插件的 `sync_now`。
//
// # 为什么不是插件自己起 ticker
//
// 插件合同禁止自建长期循环（§12），而且 wasm 单线程里 tick 内发 host_call
// 会自死锁（详见 configure 里那段注释）。
//
// # 为什么目标是 sync_now 而不是 account_sync
//
// 两者都真跑 `syncCloudDevices`，但 `sync_now` 是**家庭级**工具：
// 平台的 `runSchedule` 只对"账号级工具"要求 account 上下文
// （见 pluginsvc/events.go：非 account scope 不设置 cc.AccountID）。
// 用账号级工具做定时目标会因"创建时未盖章"直接被跳过。

// cloudSessionUsable 判断当前云会话是否**真的能用**（能解出会话密钥）。
//
// # 为什么需要它（2026-11-16）
//
// 账号状态此前只在登录那一刻写死成 ready，之后从不复查 —— token 过期或密文丢失后
// 界面仍显示绿色的「就绪」，而同步一直在失败。用户看到的是一对自相矛盾的提示。
//
// 判据分三层，任一不满足即不可用：
//  1. `cloud-session.json` 存在（否则从来没登录过）；
//  2. 里面的票据是密封的且带 secret_ref；
//  3. 该 secret_ref 能被 vault 解出来（这才是"token 还有效"的实际证明 ——
//     解不开就说明密钥材料没了/轮换过，必须重新登录）。
//
// 返回的 why 是给日志看的人话（不进 UI，UI 用 auth_required 这个状态值）。
func cloudSessionUsable(ctx *plugin.Context) (bool, string) {
	r, err := ctx.FSRead("cloud-session.json")
	if err != nil || !r.Found {
		return false, "无 cloud-session.json（从未登录）"
	}
	b, err := r.Data()
	if err != nil {
		return false, "读取 cloud-session.json 失败：" + err.Error()
	}
	var saved savedCloudSession
	if err := json.Unmarshal(b, &saved); err != nil {
		return false, "cloud-session.json 解析失败"
	}
	if !saved.TicketsSealed || saved.SecretRef == "" {
		return false, "会话票据不完整（tickets_sealed/secret_ref 缺失）"
	}
	if _, _, err := ctx.VaultBlobOpen(saved.SecretRef); err != nil {
		return false, "会话密钥不可解：" + err.Error()
	}
	/*
	 * ★ 2026-11-17（用户「没有提示过期重新登录啊」）：材料能解开 ≠ 票据还能用。
	 *
	 * 小米的 serviceToken 会过期，过期后云端一律拒绝（SERVICETOKEN_EXPIRED）。
	 * 只看"保险库能解出密钥"会让界面一直显示「就绪」，而实际同步/控制全失败。
	 *
	 * 这里再看一眼**最近一次同步的结果**：如果它因票据过期失败，就如实报出来。
	 * （自动刷新在 apiPostRC4GuestDevice 里已经做了；走到这里说明刷新也没成功，
	 * 那就真的需要用户重新登录。）
	 */
	if st := loadSyncState(ctx); isServiceTokenExpiredString(st.LastError) {
		return false, "服务票据已过期：" + st.LastError
	}
	/*
	 * 过期**前**主动预警（2026-11-17）。
	 *
	 * 实测小米 serviceToken 约 **11 小时**失效（用户 11-16 23:16 登录，
	 * 11-17 10:44 已全部云端调用被拒）。而插件无法自行续期：
	 * 换票的 Set-Cookie 被宿主 HTTP 客户端在重定向中间消费，插件侧读不到，
	 * jar 也没有因此更新（详见 absorbLocation 的注释）。
	 *
	 * 所以退而求其次：**在失效前就提醒用户**。取 9 小时（留 2 小时余量），
	 * 让用户白天就能看到「登录已过期，请重新登录」，而不是半夜发现控制失效。
	 */
	if ts, terr := time.Parse(time.RFC3339, saved.SavedAt); terr == nil {
		if age := time.Since(ts); age > 9*time.Hour {
			return false, fmt.Sprintf("登录已超过 %.0f 小时（小米票据约 11 小时失效），请重新登录", age.Hours())
		}
	}
	return true, ""
}

// isServiceTokenExpiredString 是 isServiceTokenExpired 的字符串版（供状态判据用）。
func isServiceTokenExpiredString(msg string) bool {
	if msg == "" {
		return false
	}
	u := strings.ToUpper(msg)
	return strings.Contains(u, "SERVICETOKEN") ||
		strings.Contains(u, "SERVICE_TOKEN") ||
		strings.Contains(u, "AUTH_ERR") ||
		strings.Contains(msg, "登录已过期")
}

func registerAutoSyncSchedule(ctx *plugin.Context, intervalSec int) error {
	cron, ok := cronForSeconds(intervalSec)
	if !ok {
		return fmt.Errorf("同步间隔 %ds 无法表达为平台 cron（需 >= 60s）", intervalSec)
	}
	_, err := ctx.ScheduleCreate(plugin.ScheduleCreateRequest{
		Name: autoSyncScheduleName,
		Cron: cron,
		Tool: "sync_now",
	})
	return err
}

// cronForSeconds 把"每 N 秒"翻译成平台能解析的 cron 表达式。
//
// 平台 cron 最小粒度是**分钟**，所以按区间分档，并且**向上取整**：
// 宁可少跑一次，也不要让"设 90 秒"变成每分钟跑一次（那是超额同步，
// 对小米云是压力，对用户是没必要）。
//
//	< 1 分钟   → 每分钟（下限，平台不支持秒级）
//	1..59 分钟 → 每 N 分钟
//	1..23 小时 → 每小时第 0 分、每 H 小时
//	>= 24 小时 → 每天 00:00
func cronForSeconds(intervalSec int) (string, bool) {
	if intervalSec < 60 {
		return "", false
	}
	mins := (intervalSec + 59) / 60 // 向上取整
	if mins <= 1 {
		return "* * * * *", true
	}
	if mins < 60 {
		return fmt.Sprintf("*/%d * * * *", mins), true
	}
	hours := (mins + 59) / 60
	if hours <= 1 {
		return "0 * * * *", true
	}
	if hours < 24 {
		return fmt.Sprintf("0 */%d * * *", hours), true
	}
	return "0 0 * * *", true
}

func health(*plugin.Context) (plugin.HealthStatus, error) {
	return plugin.HealthStatus{OK: true, Message: "ABI v2 资源适配已加载；业务迁移未完成"}, nil
}

var _ = time.Second
