package main

import (
	"time"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
		URL: "https://account.xiaomi.com/pass/serviceLogin?sid=xiaomiio&_json=true",
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

	// 把新票据固化：沿用正常登录的落库路径
	if err := persistReloginMaterial(ctx, hs.SessionID, deviceID, auth, user); err != nil {
		autoReloginFailures++
		return fmt.Errorf("固化新票据失败: %w", err)
	}
	autoReloginFailures = 0
	keep = true // 会话留给后续云端调用
	ctx.LogWarn("hx-mi 已用保存的账号密码自动重登成功")
	return nil
}

// persistReloginMaterial 把自动重登拿到的票据固化到会话材料与 cloud-session.json。
//
// 与正常登录流程（token_exchange.go 的 exchangeAndProbe）写的是同一份东西：
//   · 保险库 `xiaomi_session_material`：ssecurity / passToken / 账号 / userId
//   · `cloud-session.json`：给人看的字段 + secret 版本
//
// 只写这两个，不碰 account_store（账号本来就在库里，重登不是"加账号"）。
func persistReloginMaterial(ctx *plugin.Context, sessionID, deviceID string, auth authResponse, user string) error {
	// 从会话 jar 里取 ssecurity 的权威来源仍是 step1/serviceLoginAuth2 的响应；
	// 这里优先用 auth 里的值，缺省回落到保险库已有值（不覆盖成空）。
	ssec := auth.Ssecurity
	passToken := auth.PassToken
	old := cloudSecretMaterial{}
	if blob, _, err := ctx.VaultBlobOpen("xiaomi_session_material"); err == nil && len(blob) > 0 {
		_ = json.Unmarshal(blob, &old)
	}
	if ssec == "" {
		ssec = old.Ssecurity
	}
	if passToken == "" {
		passToken = old.PassToken
	}
	if ssec == "" {
		return errors.New("重登后仍未拿到 ssecurity")
	}
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
