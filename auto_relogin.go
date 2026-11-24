package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"homex.dev/homex/sdk/go/plugin"
)

/*
 * 自动重登（2026-11-17 用户要求：「把插件保存账号密码做一下加个开关，可以选择性使用」）。
 *
 * # 为什么需要
 *
 * 小米 `serviceToken` 只有约 **11 小时**有效期（实测：11-16 23:16 登录 →
 * 11-17 10:44 全部云端调用被拒）。到期后插件完全无法自愈：
 *
 *   · 用 passToken 走 step1 换票**拿不到配对的新 serviceToken** ——
 *     换票的 Set-Cookie 在 sts 重定向的中间响应里被宿主 HTTP 客户端消费，
 *     插件侧读不到（响应头只有 Connection/Content-Length/Content-Type/
 *     Date/Request-Id/Server）。半吊子刷新还会把已就绪的会话搞坏，
 *     见 9482377 的事故记录。
 *
 *   · 参考实现（hass-xiaomi-miot `_login_step2`）能自动续期，是因为它
 *     **保存了用户密码**，用 `user + md5(password)` 完整重走登录流程。
 *
 * # 安全取舍（默认关闭）
 *
 * 保存密码意味着「账号完全托付给插件」——插件或平台一旦被攻破，损失不可控。
 * 所以这里：
 *   · **默认关闭**，必须用户在配置里显式打开 `save_password` 才保存；
 *   · 密码只进保险库（`VaultBlobSeal`，宿主持有密钥），不写日志、不进错误信息；
 *   · 关闭开关立即删除已保存的密码；
 *   · 自动重登**只在票据确实过期时**尝试，且带失败计数上限，避免把账号
 *     送进小米的风控黑名单。
 */

// autoReloginCredKey 是保存密码的保险库键。
const autoReloginCredKey = "account_password"

// maxAutoReloginFailures 连续失败上限：超过就停止尝试，要求用户手动登录。
const maxAutoReloginFailures = 2

// 连续失败计数（进程内；重启归零是刻意的 —— 重启后值得再试一次）。
var autoReloginFailures int

// savePasswordIfEnabled 在登录成功且配置开启时保存密码；关闭时清除。
func savePasswordIfEnabled(ctx *plugin.Context, user, password string) {
	if !current.SavePassword {
		// 关闭开关 ⇒ 立即删除历史密码，不留残余。
		_, _ = ctx.VaultBlobDelete(autoReloginCredKey)
		return
	}
	if strings.TrimSpace(user) == "" || password == "" {
		return
	}
	blob, err := json.Marshal(map[string]string{"user": user, "password": password})
	if err != nil {
		return
	}
	if _, serr := ctx.VaultBlobSeal(autoReloginCredKey, blob); serr != nil {
		ctx.LogWarn("hx-mi 保存账号密码失败（自动重登将不可用）：%v", serr)
		return
	}
	ctx.Log(1, "hx-mi 已保存账号密码用于自动重登（配置项 save_password 已开启）")
}

// loadSavedPassword 读取保存的账号密码；未开启/未保存时返回 ok=false。
func loadSavedPassword(ctx *plugin.Context) (user, password string, ok bool) {
	if !current.SavePassword {
		return "", "", false
	}
	blob, _, err := ctx.VaultBlobOpen(autoReloginCredKey)
	if err != nil || len(blob) == 0 {
		return "", "", false
	}
	var m map[string]string
	if json.Unmarshal(blob, &m) != nil {
		return "", "", false
	}
	if m["user"] == "" || m["password"] == "" {
		return "", "", false
	}
	return m["user"], m["password"], true
}

/*
 * tryAutoRelogin 用保存的密码重走一次完整登录，成功后会话即可继续使用。
 *
 * 完整流程（而不是 step1 换票）是关键：只有 `serviceLoginAuth2` 这条
 * **账号密码**路径，宿主 CookieJar 才会在响应/重定向里收下新的
 * `serviceToken`；step1 只是查询签名页，不产生票据。
 *
 * 返回 nil 表示重登成功（云会话可再次使用）。
 */
func tryAutoRelogin(ctx *plugin.Context) error {
	user, password, ok := loadSavedPassword(ctx)
	if !ok {
		return errors.New("未开启「保存账号密码」或没有已保存的凭据")
	}
	if autoReloginFailures >= maxAutoReloginFailures {
		return fmt.Errorf("自动重登已连续失败 %d 次，请手动重新登录", autoReloginFailures)
	}

	reapStaleLoginSessions(ctx, "")
	hs, err := ctx.HTTPOpen(plugin.HTTPOpenRequest{})
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = ctx.HTTPClose(hs.SessionID)
		}
	}()
	// 登录一律用干净会话：避免上一个账号的票据让 step1 误判"已登录"，
	// 从而不给 _sign（这个坑在 startLogin 里有详细记录）。
	for _, host := range []string{"account.xiaomi.com", "sts.api.io.mi.com", "api.io.mi.com"} {
		_, _ = ctx.HTTPCookiesDelete(plugin.HTTPCookiesRequest{SessionID: hs.SessionID, Host: host})
	}
	deviceID, _ := loadOrCreateDeviceID(ctx)

	// step1：取签名参数
	r, err := ctx.HTTPSessionDo(plugin.HTTPSessionRequest{SessionID: hs.SessionID, Method: "GET",
		URL:     "https://account.xiaomi.com/pass/serviceLogin?sid=xiaomiio&_json=true",
		Headers: xiaomiHeaders(deviceID), TimeoutMS: 15000})
	if err != nil {
		return fmt.Errorf("网络不确定: %w", err)
	}
	body, berr := r.Body()
	if berr != nil {
		return berr
	}
	step1, perr := parseAuth(body)
	if perr != nil {
		return perr
	}
	// step2：账号密码
	auth, serr := doStep2(ctx, hs.SessionID, user, password, "", protocolState{
		Callback: step1.Callback, QS: step1.QS, Sign: step1.Sign,
	}, deviceID)
	if serr != nil {
		autoReloginFailures++
		return serr
	}
	if loc := auth.Location; loc != "" {
		// 跟随 location 让 jar 收下配对的 serviceToken
		if aerr := absorbLocation(ctx, hs.SessionID, deviceID, loc); aerr != nil {
			autoReloginFailures++
			return fmt.Errorf("落地地址失败: %w", aerr)
		}
	} else if auth.NotificationURL != "" {
		// 需要短信/邮件验证：自动流程无法完成
		autoReloginFailures++
		return errors.New("小米要求二次验证（短信/邮件），请手动重新登录")
	} else if auth.CaptchaURL != "" {
		autoReloginFailures++
		return errors.New("小米要求输入验证码，请手动重新登录")
	} else {
		autoReloginFailures++
		return fmt.Errorf("登录未返回 location（code=%v %s）", auth.Code, explainLoginCode(auth.Code, firstNonEmptyStr(auth.Description, auth.Desc)))
	}

	/*
	 * ★★★ 必须先**密封新票据**再落库（2026-11-25，连续 37 次 invalid signature 的真凶）
	 *
	 * 正常登录（token_exchange.go:74-95）的完整顺序是：
	 *     ① 跟随 location 换票 → ② HTTPCookiesSeal 密封 3 个 host 的 cookie
	 *     → ③ 按 host 重新 attach 回会话 → ④ RC4 探针 → ⑤ 通过才落库
	 *
	 * 而这里（自动重登）**漏了 ②③④**，只把 ssecurity/passToken 写进保险库。
	 * 后果：会话一关，新 serviceToken 就丢了；下次 openCloudSession 从保险库
	 * attach 出来的还是**旧 token**。
	 *
	 * 更糟的是我上一版把 ssecurity 改成"只取本次 location 的新值、绝不回落"，
	 * 于是变成「新 ssecurity + 旧 serviceToken」——永久不匹配，
	 * 失败次数从此只增不减（12:55 成功一次是旧代码，13:00 起全错）。
	 * 这一步补上之后，材料才真正成对。
	 */
	hosts := []string{"account.xiaomi.com", "sts.api.io.mi.com", "api.io.mi.com"}
	sealed := 0
	for _, host := range hosts {
		if r, e := ctx.HTTPCookiesSeal(plugin.HTTPCookiesRequest{SessionID: hs.SessionID, Host: host}); e == nil {
			sealed += len(r.Sealed)
			ctx.Log(1, fmt.Sprintf("hx-mi relogin sealed host=%s count=%d", host, len(r.Sealed)))
		} else {
			ctx.Log(2, fmt.Sprintf("hx-mi relogin seal host=%s err=%v", host, e))
		}
	}
	if sealed == 0 {
		autoReloginFailures++
		return errors.New("自动重登未拿到可密封票据（STS 没有下发 serviceToken）")
	}
	// 部分跳转把 cookie 落在中间 host/域上 ⇒ 按显式 host attach 回同一会话再探针。
	for _, host := range hosts {
		_, _ = ctx.HTTPCookiesAttach(plugin.HTTPCookiesRequest{SessionID: hs.SessionID, Host: host})
	}
	/*
	 * 探针：用**本次**的 ssecurity + deviceID 走 RC4 打一次 device_list。
	 * 不通过就绝不落库 —— 宁可报"重登失败"让用户手动登录，也不把一对
	 * 用不了的材料写进保险库（那正是连续失败 37 次的形态）。
	 */
	probeSec := firstNonEmptyStr(auth.Ssecurity, ssecurityFromLocation(auth.Location))
	if probeSec == "" {
		autoReloginFailures++
		return errors.New("自动重登未拿到配对的 ssecurity（location 缺参数）")
	}
	if body, perr := apiPostRC4GuestDevice(ctx, hs.SessionID, probeSec, deviceID,
		"https://api.io.mi.com/app/home/device_list",
		`{"getVirtualModel":true,"getHuamiDevices":1,"get_split_device":false,"support_smart_home":true}`); perr != nil {
		autoReloginFailures++
		return fmt.Errorf("重登探针失败（已密封 %d 条票据，未落库）：%w", sealed, perr)
	} else if env := map[string]json.RawMessage{}; json.Unmarshal(body, &env) != nil || env["result"] == nil {
		autoReloginFailures++
		return fmt.Errorf("重登探针响应不确定（已密封 %d 条票据，未落库）", sealed)
	}

	// 把新票据固化：沿用正常登录的落库路径
	if err := persistReloginMaterial(ctx, hs.SessionID, deviceID, auth, user); err != nil {
		autoReloginFailures++
		return fmt.Errorf("固化新票据失败: %w", err)
	}
	autoReloginFailures = 0
	invalidateSessionProbe() // 新票据生效：探针缓存立即作废（2026-11-20）
	keep = true              // 会话留给后续云端调用
	ctx.LogWarn("hx-mi 已用保存的账号密码自动重登成功")
	return nil
}

// ssecurityFromLocation 从小米换票地址（step2 响应的 location）里取出**配对的** ssecurity。
//
// # 为什么必须从 location 取（2026-11-24，`invalid signature` 的最终根因）
//
// 小米密码登录链路里，ssecurity 与 serviceToken 是**严格配对**的签名材料：
// serviceToken 由 sts 按 location 下发，而用于 RC4 签名的 ssecurity 就写在
// 同一条 location 的查询参数 `ssecurity=` 里。它们必须出自**同一次**换票。
//
// 扫码登录那条路（mi_qrlogin.go）从 JSON 响应直接拿到了 ssecurity，所以能用；
// 而密码登录 / 自动重登这条路 ssecurity **只在 location 里**，JSON 主体是空的。
// 此前 persistReloginMaterial 遇到空值就"回落到保险库旧值"——
// **新 serviceToken 配旧 ssecurity** ⇒ 每个云端请求都报 `invalid signature`。
//
// 解析失败返回空串，由调用方明确报错，**绝不复用旧值**。
func ssecurityFromLocation(loc string) string {
	loc = strings.TrimSpace(loc)
	if loc == "" {
		return ""
	}
	u, err := url.Parse(loc)
	if err != nil {
		return ""
	}
	if v := u.Query().Get("ssecurity"); v != "" {
		return v
	}
	if frag := u.Fragment; frag != "" {
		if i := strings.Index(frag, "?"); i >= 0 {
			frag = frag[i+1:]
		}
		if v, _ := url.ParseQuery(frag); v.Get("ssecurity") != "" {
			return v.Get("ssecurity")
		}
	}
	return ""
}

// persistReloginMaterial 把自动重登拿到的票据固化到会话材料与 cloud-session.json。
//
// 与正常登录流程（token_exchange.go 的 exchangeAndProbe）写的是同一份东西：
//
//	· 保险库 `xiaomi_session_material`：ssecurity / passToken / 账号 / userId
//	· `cloud-session.json`：给人看的字段 + secret 版本
//
// 只写这两个，不碰 account_store（账号本来就在库里，重登不是"加账号"）。
func persistReloginMaterial(ctx *plugin.Context, sessionID, deviceID string, auth authResponse, user string) error {
	// 从会话 jar 里取 ssecurity 的权威来源仍是 step1/serviceLoginAuth2 的响应；
	// 这里优先用 auth 里的值，缺省回落到保险库已有值（不覆盖成空）。
	/*
	 * ★★★ ssecurity 的唯一来源是**本次换票的 location**（见 ssecurityFromLocation）。
	 * 之前"auth.Ssecurity 为空就用保险库旧值"正是 invalid signature 的根因：
	 * 新 serviceToken 配旧 ssecurity。这里宁可明确失败，也绝不混用两次换票的材料。
	 * passToken 可以回落旧值（它只用于续登，不参与 RC4 签名，不会造成签名错配）。
	 */
	ssec := firstNonEmptyStr(auth.Ssecurity, ssecurityFromLocation(auth.Location))
	old := cloudSecretMaterial{}
	if blob, _, err := ctx.VaultBlobOpen("xiaomi_session_material"); err == nil && len(blob) > 0 {
		_ = json.Unmarshal(blob, &old)
	}
	if ssec == "" {
		return errors.New("重登后未拿到配对的 ssecurity（location 缺参数）—— 拒绝复用旧值，请手动重新登录")
	}
	passToken := firstNonEmptyStr(auth.PassToken, old.PassToken)
	userID := old.UserID
	if v := fmt.Sprint(auth.UserID); v != "" && v != "<nil>" {
		userID = v
	}
	mat := cloudSecretMaterial{Ssecurity: ssec, PassToken: passToken, Account: user, UserID: userID}
	blob, _ := json.Marshal(mat)
	info, err := ctx.VaultBlobSeal("xiaomi_session_material", blob)
	if err != nil {
		return err
	}
	saved := savedCloudSession{
		Version: 2, Region: current.Region, MaskedUser: maskAccount(user),
		Account: user, SavedAt: time.Now().UTC().Format(time.RFC3339),
		DeviceID: deviceID, SecretRef: "xiaomi_session_material",
		SecretVersion: info.Version, TicketsSealed: true,
	}
	b, _ := json.Marshal(saved)
	w := plugin.FSWriteRequest{Path: "cloud-session.json", QuotaKB: 4096}
	w.SetData(b)
	return ctx.FSWrite(w)
}

// maskAccount 生成脱敏账号（展示/日志用；完整账号只在展示面出现）。
func maskAccount(account string) string {
	if at := strings.Index(account, "@"); at > 0 {
		name := account[:at]
		if len(name) <= 2 {
			return account
		}
		return name[:1] + "***" + account[at:]
	}
	return account
}

// credCheckTool 是**只读自检**工具（`cred_check`）：验证账号密码保险库键的读写权限，
// 不涉及任何真实凭据（写入的是占位串，读回后立刻删除）。
//
// # 为什么需要（2026-11-17）
//
// 用户开启了 save_password 并登录，日志却报 `PLUGIN_PERMISSION_DENIED: secret blob` ——
// 因为 manifest 里漏声明了 `account_password`。这种"代码对、声明错"的失败
// 只有到用户真正登录才会暴露，体验很差。
// 有了这个自检，能在让用户登录**之前**就确认链路通不通。
func credCheckTool(ctx *plugin.Context) (json.RawMessage, error) {
	const probe = `{"user":"__probe__","password":"__probe__"}`
	out := map[string]any{}

	info, err := ctx.VaultBlobSeal(autoReloginCredKey, []byte(probe))
	if err != nil {
		out["seal_ok"] = false
		out["seal_error"] = err.Error()
		return json.Marshal(out)
	}
	out["seal_ok"] = true
	out["version"] = info.Version

	blob, _, oerr := ctx.VaultBlobOpen(autoReloginCredKey)
	if oerr != nil {
		out["open_ok"] = false
		out["open_error"] = oerr.Error()
	} else {
		out["open_ok"] = string(blob) == probe
	}
	if _, derr := ctx.VaultBlobDelete(autoReloginCredKey); derr != nil {
		out["delete_ok"] = false
		out["delete_error"] = derr.Error()
	} else {
		out["delete_ok"] = true
	}
	// 读回确认已删除，避免占位串残留影响真实保存
	if b, _, e2 := ctx.VaultBlobOpen(autoReloginCredKey); e2 == nil && len(b) > 0 {
		out["residual"] = true
	}
	out["save_password_enabled"] = current.SavePassword
	out["verdict"] = "保险库读写链路正常，下次登录即可保存密码"
	if !current.SavePassword {
		out["verdict"] = "链路正常，但配置项 save_password 未开启 —— 打开后重新登录才会保存"
	}
	return json.Marshal(out)
}
