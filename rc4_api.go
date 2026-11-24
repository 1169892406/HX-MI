package main

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"homex.dev/homex/sdk/go/plugin"
)

type orderedKV struct{ K, V string }

func genNonceAt(now time.Time) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b[:8]); err != nil {
		return "", err
	}
	binary.BigEndian.PutUint32(b[8:], uint32(now.UnixMilli()/60000))
	return base64.StdEncoding.EncodeToString(b), nil
}
func signedNonceKey2(ssecurity, nonce string) (string, error) {
	ss, e := base64.StdEncoding.DecodeString(ssecurity)
	if e != nil {
		return "", e
	}
	nn, e := base64.StdEncoding.DecodeString(nonce)
	if e != nil {
		return "", e
	}
	sum := sha256.Sum256(append(append([]byte{}, ss...), nn...))
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}
func rc4Value(signed, plain string) (string, error) {
	key, e := base64.StdEncoding.DecodeString(signed)
	if e != nil {
		return "", e
	}
	return base64.StdEncoding.EncodeToString(rc4Crypt(key, []byte(plain))), nil
}
func rc4Sign(method, rawURL string, params []orderedKV, signed string) string {
	u, _ := url.Parse(rawURL)
	p := u.Path
	if strings.HasPrefix(p, "/app/") {
		p = p[4:]
	}
	parts := []string{strings.ToUpper(method), p}
	for _, v := range params {
		parts = append(parts, v.K+"="+v.V)
	}
	parts = append(parts, signed)
	sum := sha1.Sum([]byte(strings.Join(parts, "&")))
	return base64.StdEncoding.EncodeToString(sum[:])
}
func buildRC4Body(apiURL, ssecurity, data, nonce string) (url.Values, string, error) {
	signed, e := signedNonceKey2(ssecurity, nonce)
	if e != nil {
		return nil, "", e
	}
	plain := []orderedKV{{"data", data}}
	plain = append(plain, orderedKV{"rc4_hash__", rc4Sign("POST", apiURL, plain, signed)})
	enc := make([]orderedKV, 0, len(plain))
	for _, p := range plain {
		v, e := rc4Value(signed, p.V)
		if e != nil {
			return nil, "", e
		}
		enc = append(enc, orderedKV{p.K, v})
	}
	form := url.Values{}
	for _, p := range enc {
		form.Set(p.K, p.V)
	}
	form.Set("signature", rc4Sign("POST", apiURL, enc, signed))
	form.Set("ssecurity", ssecurity)
	form.Set("_nonce", nonce)
	return form, signed, nil
}
func apiPostRC4Guest(ctx *plugin.Context, sessionID, ssecurity, apiURL, data string) ([]byte, error) {
	return apiPostRC4GuestDevice(ctx, sessionID, ssecurity, "", apiURL, data)
}

// apiPostRC4GuestDevice 额外携带小米设备标识。
//
// 旧版 protocolClient 对每个请求（含 device_list）都带 deviceId/sdkVersion；
// 新版探针之前没带，小米会以 401 拒绝已换到的 serviceToken。deviceID 为空时
// 保持旧行为（历史调用点不受影响）。
/*
 * apiPostRC4GuestDevice 是所有云端调用的唯一出口（设备列表、prop/get、prop/set、action）。
 *
 * ★★★ 2026-11-17（用户「没有提示过期重新登录啊」）：
 *
 * 小米的 **serviceToken 会过期**，过期后云端一律拒绝：
 * `SERVICETOKEN_EXPIRED`。而本插件此前**从不刷新它** —— 一旦过期，
 * 同步、控制、状态读取全部失败，只能靠用户重新登录。
 * 界面上还显示"就绪"（因为会话材料本身能解开），这就是用户看到的矛盾。
 *
 * 参考实现的做法（hass-xiaomi-miot `async_check_auth`）：
 * 检测到 token 过期 → `async_relogin()` 用缓存的 passToken 自动重登 → 重试原请求。
 * 本插件其实**已经有**那条流程（`refreshPairedMaterial`：step1 + sts location），
 * 只是云同步路径从来不调用它。
 *
 * 现在：过期时用 passToken 静默刷新一次并重试；刷新也失败才把错误抛给上层
 * （那时账号状态会变成「登录已过期，请重新登录」）。
 */
func apiPostRC4GuestDevice(ctx *plugin.Context, sessionID, ssecurity, deviceID, apiURL, data string) ([]byte, error) {
	if ssecurity == "" {
		return nil, errors.New("缺少 ssecurity")
	}
	/*
	 * ⚠⚠ 这里**故意不做自动刷新**（2026-11-17 事故后移除）。
	 *
	 * 曾经实现过"检测到 SERVICETOKEN_EXPIRED → 用 passToken 换票 → 重试"，
	 * 结果**把本来可用的会话搞坏了**：
	 *
	 *   step1 能拿到新的 `ssecurity`，但与之配对的 `serviceToken` 是通过
	 *   sts 重定向下发的 Set-Cookie —— 宿主 HTTP 客户端在中间响应里就消费掉了，
	 *   插件侧读不到（实测响应头只有 Connection/Content-Length/Content-Type/
	 *   Date/Request-Id/Server，**没有 Set-Cookie**）。于是 jar 里仍是旧
	 *   serviceToken，而我把新 ssecurity 写回了保险库 ⇒ **两者不配对**，
	 *   原本还能用的会话被彻底废掉，之后所有云端调用都报 SERVICETOKEN_EXPIRED。
	 *
	 * 教训：**没有能力验证配对成功时，宁可不刷新**。
	 *
	 * ## 但用户要求「保存账号密码 + 开关」（2026-11-17）
	 *
	 * 有密码时情况不同：可以走**完整登录流程**（serviceLoginAuth2），
	 * 那条路宿主 CookieJar 才会收下新的 serviceToken —— 与 step1 换票不是一回事。
	 * 所以这里：票据过期 → 若配置开启且存了密码 → 完整重登一次 → 重试。
	 * 没有密码就如实报错，由 9 小时判据提前提醒用户。
	 */
	raw, err := apiPostRC4GuestDeviceOnce(ctx, sessionID, ssecurity, deviceID, apiURL, data)
	if err == nil || !isServiceTokenExpired(err) {
		return raw, err
	}
	if _, _, ok := loadSavedPassword(ctx); !ok {
		return raw, err // 未开启保存密码：不尝试，如实上报
	}
	if rerr := tryAutoRelogin(ctx); rerr != nil {
		ctx.LogWarn("hx-mi 自动重登失败：%v", rerr)
		return raw, err
	}
	// 重登后会话材料已更新：用新的 ssecurity 重试一次。
	_, newSSec, _, oerr := openCloudSession(ctx)
	if oerr != nil || newSSec == "" {
		return raw, err
	}
	return apiPostRC4GuestDeviceOnce(ctx, sessionID, newSSec, deviceID, apiURL, data)
}

// isServiceTokenExpired 判断错误是否是"票据失效"（需要刷新/重登）。
//
// ★★★ 2026-11-25（用户「连续失败 37 次：invalid signature」的真凶）
//
// 这里原来只认 SERVICETOKEN / SERVICE_TOKEN / AUTH_ERR / 登录已过期 四种。
// 而 **`invalid signature` 不在其中**，于是：
//
//	· 每 2.5 分钟的自动同步拿到 `invalid signature`；
//	· 被判成"不是票据问题"⇒ **永不触发自动重登**；
//	· 于是一直报错、越积越多（用户看到 37 次），而重登一次都没被尝试过。
//
// `invalid signature` 的语义恰恰就是「ssecurity 与 serviceToken 不配对」——
// 和"票据过期"属于同一类故障，都必须重登才能修。补进来之后才有自愈能力。
//
// 同时认 `auth error`：小米在 serviceToken 无效时也用它。
func isServiceTokenExpired(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "SERVICETOKEN") ||
		strings.Contains(msg, "SERVICE_TOKEN") ||
		strings.Contains(msg, "AUTH_ERR") ||
		strings.Contains(msg, "AUTH ERROR") ||
		strings.Contains(msg, "INVALID SIGNATURE") ||
		strings.Contains(msg, "登录已过期")
}

// refreshCloudTokens 用保险库里的 passToken 换新 (ssecurity, serviceToken) 并落库。
//
// 复用登录流程里已验证过的 refreshPairedMaterial（step1 serviceLogin + sts location），
// 它从 vault 读 passToken 自建 Cookie 头，**不依赖用户在场**。
func refreshCloudTokens(ctx *plugin.Context, sessionID, deviceID string) (string, error) {
	/*
	 * ⚠ 不用 `refreshPairedMaterial`（2026-11-17 实测它是**假成功**）。
	 *
	 * 那个函数在 `loc == ""` 或 loc 不在可信范围时**静默跳过**换票，
	 * 但仍返回非空 ssecurity —— 调用方会以为"刷新成功"，于是拿旧
	 * serviceToken 重试，结果还是 SERVICETOKEN_EXPIRED。
	 * 日志实证：
	 *	服务票据已过期，已用 passToken 自动刷新并重试   ← 以为成功
	 *	sync_now 失败：SERVICETOKEN_EXPIRED            ← 重试仍失败
	 *
	 * 这里走**严格三步**，任何一步没做到都明确报错，绝不假装成功：
	 *  ① step1 serviceLogin 拿 ssecurity + location；
	 *  ② GET location 让宿主 CookieJar 收下**与该 ssecurity 配对的新 serviceToken**；
	 *  ③ 新 ssecurity 落回保险库。
	 */
	ssec, pt2, loc, err := step1Ssecurity(ctx, sessionID, deviceID)
	if err != nil {
		return "", fmt.Errorf("step1 失败：%w", err)
	}
	// 诊断：passToken 是否还在、location 指向哪、ssecurity 长度。
	ctx.LogWarn("hx-mi step1 结果：ssec长度=%d passToken=%v loc前缀=%s",
		len(ssec), pt2 != "", clipForLog(loc, 90))
	if ssec == "" {
		return "", errors.New("step1 未返回 ssecurity（passToken 可能已失效）")
	}
	if loc == "" {
		return "", errors.New("step1 未返回 sts location（无法换取新 serviceToken）")
	}
	if !allowedVerifyURL(loc) && !strings.HasPrefix(loc, "https://sts.api.io.mi.com/") {
		return "", fmt.Errorf("sts location 不在可信范围：%s", loc)
	}
	if aerr := absorbLocation(ctx, sessionID, deviceID, loc); aerr != nil {
		return "", fmt.Errorf("换 serviceToken 失败：%w", aerr)
	}
	ctx.LogWarn("hx-mi 服务票据已刷新（step1+sts 均成功）")
	// 把新 ssecurity 写回会话材料，否则下次调用还是旧的。
	mat, _, oerr := ctx.VaultBlobOpen("xiaomi_session_material")
	if oerr == nil {
		var m cloudSecretMaterial
		if json.Unmarshal(mat, &m) == nil {
			m.Ssecurity = ssec
			if b, merr := json.Marshal(m); merr == nil {
				if _, serr := ctx.VaultBlobSeal("xiaomi_session_material", b); serr != nil {
					ctx.LogWarn("hx-mi 刷新后的 ssecurity 落库失败（本次仍可用）：%v", serr)
				}
			}
		}
	}
	return ssec, nil
}

// apiPostRC4GuestDeviceOnce 是原来的单次实现（重试逻辑在外面）。
func apiPostRC4GuestDeviceOnce(ctx *plugin.Context, sessionID, ssecurity, deviceID, apiURL, data string) ([]byte, error) {
	if ssecurity == "" {
		return nil, errors.New("缺少 ssecurity")
	}
	nonce, e := genNonceAt(time.Now())
	if e != nil {
		return nil, e
	}
	form, signed, e := buildRC4Body(apiURL, ssecurity, data, nonce)
	if e != nil {
		return nil, e
	}
	headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept-Encoding": "identity", "MIOT-ENCRYPT-ALGORITHM": "ENCRYPT-RC4", "X-XIAOMI-PROTOCAL-FLAG-CLI": "PROTOCAL-HTTP2", "User-Agent": "Android-7.1.1-1.0.0-ONEPLUS A3010-136-"}
	// 旧版/上游对每个业务请求都显式带一组静态 cookie（locale/channel/timezone/
	// is_daylight/dst_offset）。它们不是服务端下发的，jar 里没有；缺了会被小米
	// 判为 auth error（返回明文错误信封）。yetAnotherServiceToken/serviceToken/
	// userId/cUserId 由宿主票据 attach 注入 jar，Go http.Client 会把 jar cookie
	// 追加到手工 Cookie 头之后，两者并存。
	staticCookies := "locale=zh_CN; channel=MI_APP_STORE; timezone=GMT+08:00; is_daylight=0; dst_offset=0"
	if deviceID != "" {
		headers["User-Agent"] = fmt.Sprintf(xiaomiUATemplate, deviceID)
		headers["Cookie"] = "deviceId=" + deviceID + "; sdkVersion=" + xiaomiSDKVersion + "; " + staticCookies
	} else {
		headers["Cookie"] = staticCookies
	}
	req := plugin.HTTPSessionRequest{SessionID: sessionID, Method: "POST", URL: apiURL, Headers: headers, TimeoutMS: 20000}
	req.SetBody([]byte(form.Encode()))
	resp, e := ctx.HTTPSessionDo(req)
	if e != nil {
		return nil, fmt.Errorf("网络不确定: %w", e)
	}
	body, e := resp.Body()
	if e != nil {
		return nil, e
	}
	if resp.Status == 401 || resp.Status == 403 {
		return nil, fmt.Errorf("认证票据被明确拒绝（host=%s，status=%d）", hostOf(apiURL), resp.Status)
	}
	if resp.Status >= 500 {
		return nil, fmt.Errorf("网络不确定: HTTP %d", resp.Status)
	}
	// 小米的成功响应是 **base64 文本**形式的 RC4 密文（旧版 decryptRC4Value
	// 先 base64 解码再 RC4）；此前直接把 wire 字节当密文是错的，会在认证
	// 修好后立刻变成"响应不确定"。明文 JSON 则是业务错误信封——必须把
	// code/message 透出来：盲报"错误信封"让排查绕了远路（信封里的原文
	// 才是小米的真实抱怨，例如 auth error / 签名不符 / 频控）。
	text := strings.TrimSpace(string(body))
	if strings.HasPrefix(text, "{") {
		var er struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
			Desc    string `json:"desc"`
		}
		if json.Unmarshal([]byte(text), &er) == nil && (er.Message != "" || er.Desc != "" || er.Code != nil) {
			detail := firstNonEmpty(er.Message, er.Desc)
			if len(detail) > 160 {
				detail = detail[:160]
			}
			return nil, fmt.Errorf("小米业务接口拒绝（code=%v，说明：%s）", er.Code, detail)
		}
		return nil, errors.New("业务接口返回未加密 JSON，拒绝作为成功响应")
	}
	out, derr := decryptRC4Text(signed, text)
	if derr != nil {
		/*
		 * 诊断容错（2026-11-17）：响应既不是明文 JSON 也不是合法 base64，
		 * 说明命中的是**不存在的路径**或非 API 端点（404 文本/HTML）。
		 * 把原文带出来才能判断接口不存在还是接口存在但没权限，
		 * 否则永远只看到 illegal base64 这种无用信息。
		 */
		snip := text
		if len(snip) > 200 {
			snip = snip[:200] + "…"
		}
		return nil, fmt.Errorf("响应既非 JSON 也非 base64（%v）；原文前 200 字：%s", derr, snip)
	}
	return out, nil
}
func decryptRC4Bytes(signed string, cipherBytes []byte) ([]byte, error) {
	key, e := base64.StdEncoding.DecodeString(signed)
	if e != nil {
		return nil, e
	}
	if len(cipherBytes) == 0 {
		return nil, errors.New("RC4 响应密文为空")
	}
	return rc4Crypt(key, cipherBytes), nil
}

// decryptRC4Text 仅供明确拿到上游 base64 文本的兼容/测试路径。
func decryptRC4Text(signed, cipherText string) ([]byte, error) {
	raw, e := base64.StdEncoding.DecodeString(strings.TrimSpace(cipherText))
	if e != nil {
		return nil, e
	}
	return decryptRC4Bytes(signed, raw)
}
