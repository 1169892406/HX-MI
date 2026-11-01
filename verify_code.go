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

// 小米身份验证渠道标志（与上游 hass-xiaomi-miot 一致）：4=短信，8=邮件。
const (
	verifyFlagPhone = "4"
	verifyFlagEmail = "8"
)

// identityOptions 取当前会话可用的验证渠道，并让宿主 CookieJar 收下
// identity_session。
//
// 旧版（Go 服务）验证码之所以能用，是因为它在提交前先 GET 了 identity/list：
//   - 路径来源：把 verifyURL 里的 fe/service/identity/authStart 替换成 identity/list，
//     query 原样保留；
//   - 响应会下发 identity_session cookie，提交验证码时必须带上，否则小米回 10012
//     “非法请求”，用户会误以为验证码错。
//
// 宿主 HTTP 会话自带 cookie jar，因此这里不需要手工搬 cookie；只要同会话请求即可。
// absorbLocation 访问验证落地地址，把 serviceToken 等 Cookie 收进宿主会话。
func absorbLocation(ctx *plugin.Context, hostSessionID, _ string, loc string) error {
	if !allowedVerifyURL(loc) && !strings.HasPrefix(loc, "https://sts.api.io.mi.com/") {
		return errors.New("落地地址不在可信范围")
	}
	// 收票请求**只带会话 jar 里的 cookies**：旧版 absorbLocation 同样不带
	// deviceId/sdkVersion 头。显式 Cookie 头会与 jar 合并/覆盖，实测把刚下发的
	// 票据挤掉，返回 401“认证票据被明确拒绝”。
	resp, err := ctx.HTTPSessionDo(plugin.HTTPSessionRequest{SessionID: hostSessionID, Method: "GET", URL: loc, TimeoutMS: 20000})
	if err != nil {
		return err
	}
	if resp.Status == 401 || resp.Status == 403 {
		return errors.New("落地地址拒绝了当前会话")
	}
	if resp.Status < 200 || resp.Status >= 500 {
		return fmt.Errorf("落地地址返回 HTTP %d", resp.Status)
	}
	return nil
}

func snippet(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

func identityOptions(ctx *plugin.Context, hostSessionID, deviceID, verifyURL string) ([]string, error) {
	if strings.TrimSpace(verifyURL) == "" {
		return nil, errors.New("当前会话没有验证入口地址")
	}
	listURL := strings.Replace(verifyURL, "fe/service/identity/authStart", "identity/list", 1)
	if listURL == verifyURL {
		return nil, errors.New("验证入口不是 authStart 形态，无法获取验证方式")
	}
	resp, err := ctx.HTTPSessionDo(plugin.HTTPSessionRequest{SessionID: hostSessionID, Method: "GET", URL: listURL, Headers: xiaomiHeaders(deviceID), TimeoutMS: 15000})
	if err != nil {
		return nil, fmt.Errorf("获取验证方式失败: %w", err)
	}
	body, err := resp.Body()
	if err != nil {
		return nil, err
	}
	// 宿主 HTTP 会话不跨进程持久化：服务重启后 login-sessions 里的
	// host_session_id 已失效，identity/list 会退回登录页 HTML。必须把这种
	// 情况说成“会话过期、请重新登录”，而不是让用户对着“非法 JSON”猜。
	if strings.HasPrefix(strings.TrimSpace(string(body)), "<") {
		return nil, errors.New("登录会话已失效（小米返回登录页）；请重新输入账号密码登录")
	}
	// options 实际是**数字数组**（如 [4]、[4,8]），早期按 []string 解析会
	// 直接报“无法解析”，把协议差异伪装成响应损坏。统一用 any 再归一化。
	var v struct {
		Flag    any   `json:"flag"`
		Options []any `json:"options"`
	}
	if err := json.Unmarshal(stripJSONP(body), &v); err != nil {
		return nil, fmt.Errorf("验证方式响应无法解析（%v；前128字节：%s）", err, snippet(body, 128))
	}
	flags := make([]string, 0, len(v.Options))
	for _, o := range v.Options {
		switch x := o.(type) {
		case string:
			if s := strings.TrimSpace(x); s != "" {
				flags = append(flags, s)
			}
		case float64:
			flags = append(flags, fmt.Sprintf("%.0f", x))
		}
	}
	if len(flags) == 0 {
		switch f := v.Flag.(type) {
		case float64:
			if f != 0 {
				flags = []string{fmt.Sprintf("%.0f", f)}
			}
		case string:
			if strings.TrimSpace(f) != "" {
				flags = []string{strings.TrimSpace(f)}
			}
		}
	}
	return flags, nil
}

// submitVerifyTicket 把验证码提交给小米对应渠道。
//
// 形状与上游 verify_ticket 一致：_json/trust/_flag/ticket 全在 form data，
// query 只带 _dc 时间戳；qs 来自登录协议状态。
func submitVerifyTicket(ctx *plugin.Context, hostSessionID, deviceID, qs, flag, code string) (location, detail string, ok bool) {
	path := "/identity/auth/verifyPhone"
	if flag == verifyFlagEmail {
		path = "/identity/auth/verifyEmail"
	}
	form := url.Values{}
	form.Set("_flag", flag)
	form.Set("ticket", strings.TrimSpace(code))
	form.Set("trust", "false")
	form.Set("_json", "true")
	if qs != "" {
		form.Set("qs", qs)
	}
	target := "https://account.xiaomi.com" + path + "?_dc=" + fmt.Sprintf("%d", time.Now().UnixMilli())
	req := plugin.HTTPSessionRequest{
		SessionID: hostSessionID, Method: "POST", URL: target,
		Headers: mergeHeaders(map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, xiaomiHeaders(deviceID)),
		TimeoutMS: 15000,
	}
	req.SetBody([]byte(form.Encode()))
	resp, err := ctx.HTTPSessionDo(req)
	if err != nil {
		return "", "网络不确定", false
	}
	body, err := resp.Body()
	if err != nil {
		return "", "响应读取失败", false
	}
	var v struct {
		Code     any    `json:"code"`
		Desc     string `json:"desc"`
		Message  string `json:"message"`
		Location string `json:"location"`
	}
	_ = json.Unmarshal(stripJSONP(body), &v)
	codeStr := ""
	switch c := v.Code.(type) {
	case float64:
		codeStr = fmt.Sprintf("%.0f", c)
	case string:
		codeStr = strings.TrimSpace(c)
	}
	if codeStr == "" || codeStr == "0" {
		return v.Location, "", true
	}
	detail = strings.TrimSpace(v.Desc)
	if detail == "" {
		detail = strings.TrimSpace(v.Message)
	}
	if detail == "" {
		detail = "code=" + codeStr
	}
	if len(detail) > 160 {
		detail = detail[:160] + "…"
	}
	return "", detail, false
}

// step1Ssecurity 在验证通过后重新取一次登录签名页，拿 ssecurity/passToken。
//
// 旧版明确警告：验证后不要重走 step2（会再次触发验证）；只取 step1 的
// ssecurity/passToken，然后直接用验证响应的 location 换票据。
// step1Ssecurity 在验证通过后重新取一次登录签名页，拿 ssecurity/passToken/location。
//
// # 为什么必须连 location 一起返回（invalid signature 的根因）
//
// 小米对“已认证会话”的 serviceLogin 每次都下发**一对新材料**：
// 新 ssecurity + 新 STS location；GET 该 location 才会把与这个 ssecurity
// **配对**的 serviceToken 写进 jar。只取 ssecurity 而丢弃 location，探针
// 就会用新钥匙签旧票 —— 小米返回 {"code":0,"message":"invalid signature"}
// （已用真实账号实测钉住：配对材料探针 OK，错配材料必然 invalid signature）。
// step1Ssecurity 用**已保存的凭据**重新取一对会话材料。
//
// # 为什么必须带 passToken + userId（invalid signature 的最终根因）
//
// 小米的 serviceLogin 只有在请求里带 passToken（+userId）时才认"这是已登录的
// 同一个账号"，并下发**与账号会话配对**的 (ssecurity, STS location)。
// 只带 deviceId+sdkVersion 时，它给的是一份没有会话归属的材料：location 甚至
// 换不出 serviceToken（真实账号对照实验：带 passToken → PROBE-OK；
// 不带 → NO-TOKEN），而插件此前正是"不带"，于是拿到的 ssecurity 与 jar 里
// 最终落地的 token 不是同一对 —— 探针报 {"code":0,"message":"invalid signature"}。
//
// 旧版 resumeAfterVerify 之所以没这个毛病：它的 step1Full 走的是同一个
// 带凭据的 HTTP 客户端（jar 里已有 passToken）。
func step1Ssecurity(ctx *plugin.Context, hostSessionID, deviceID string) (ssecurity, passToken, location string, err error) {
	// 把保险库里的 passToken/userId 显式放进请求头：宿主 jar 未必持有一份
	// （登录后票据是 sealed 在 HostVault 里，attach 只还原 cookie 名，
	// passToken 属于会话材料而非 cookie 票据时就不在 jar 中）。
	headers := xiaomiHeaders(deviceID)
	if mat, _, merr := ctx.VaultBlobOpen("xiaomi_session_material"); merr == nil {
		var material cloudSecretMaterial
		if json.Unmarshal(mat, &material) == nil && material.PassToken != "" {
			ck := ""
			if headers != nil {
				ck = headers["Cookie"]
			}
			ck = "passToken=" + material.PassToken + "; " + ck
			id := loadAccountIdentity(ctx)
			if id != nil && id.UserID != "" {
				ck = "userId=" + id.UserID + "; " + ck
			}
			if headers == nil {
				headers = map[string]string{}
			}
			headers["Cookie"] = ck
		}
	}
	resp, err := ctx.HTTPSessionDo(plugin.HTTPSessionRequest{
		SessionID: hostSessionID, Method: "GET",
		URL: "https://account.xiaomi.com/pass/serviceLogin?sid=xiaomiio&_json=true",
		Headers: headers, TimeoutMS: 15000,
	})
	if err != nil {
		return "", "", "", err
	}
	body, err := resp.Body()
	if err != nil {
		return "", "", "", err
	}
	auth, err := parseAuth(body)
	if err != nil {
		return "", "", "", err
	}
	return auth.Ssecurity, auth.PassToken, auth.Location, nil
}

// refreshPairedMaterial 取“配对的”会话材料：step1 拿 (ssecurity, location)，
// 再 GET location 让 jar 收下与该 ssecurity 配对的 serviceToken。
// 这是 micloud/_login_step3 的流程，已用真实账号验证（PAIR-B PROBE OK）。
func refreshPairedMaterial(ctx *plugin.Context, hostSessionID, deviceID string) (ssecurity, passToken string, err error) {
	ssec, pt, loc, e := step1Ssecurity(ctx, hostSessionID, deviceID)
	if e != nil {
		return "", "", e
	}
	if ssec == "" {
		return "", pt, nil
	}
	if loc != "" && (allowedVerifyURL(loc) || strings.HasPrefix(loc, "https://sts.api.io.mi.com/")) {
		if ae := absorbLocation(ctx, hostSessionID, deviceID, loc); ae != nil {
			return "", "", fmt.Errorf("配对票据交换失败: %w", ae)
		}
		ctx.Log(1, "hx-mi paired material refreshed (step1+sts)")
	}
	return ssec, pt, nil
}

// verifyByCode 是验证码渠道的完整流程：取渠道 → 提交 → 换票据 → 探针 → 落库。
func verifyByCode(ctx *plugin.Context, id string, s *loginSession, code string) (json.RawMessage, error) {
	if s.Protocol.Challenge == nil || s.Protocol.Challenge.VerifyURL == "" {
		return nil, errors.New("当前会话不需要短信/邮件验证码")
	}
	if s.Attempts >= 5 {
		return nil, errors.New("验证码尝试次数过多，请重新登录")
	}
	if _, e := ctx.HTTPInfo(s.HostSessionID); e != nil {
		return nil, errors.New("登录会话已失效（服务重启或超时）；请重新输入账号密码登录")
	}
	s.Attempts++
	_ = saveLoginSession(ctx, id, *s)

	deviceID := s.DeviceID
	if deviceID == "" {
		deviceID = randomDeviceID()
		s.DeviceID = deviceID
		_ = saveLoginSession(ctx, id, *s)
	}
	flags, err := identityOptions(ctx, s.HostSessionID, deviceID, s.Protocol.Challenge.VerifyURL)
	if err != nil {
		return nil, err
	}
	if len(flags) == 0 {
		flags = []string{verifyFlagPhone, verifyFlagEmail}
	}
	var lastDetail string
	for _, flag := range flags {
		location, detail, ok := submitVerifyTicket(ctx, s.HostSessionID, deviceID, s.Protocol.QS, flag, code)
		if ok {
			// 顺序不能反：先访问 location 让宿主 CookieJar 收下 serviceToken，
			// 再取 step1 的 ssecurity。旧版 resumeAfterVerify 也是这个顺序；
			// 反过来会出现“serviceToken 已到手，但 ssecurity 为空”，随后
			// 设备探针报“缺少 ssecurity”。
			if location != "" {
				if err := absorbLocation(ctx, s.HostSessionID, s.DeviceID, location); err != nil {
					return nil, fmt.Errorf("验证已通过，但交换服务票据失败: %w", err)
				}
			}
			ssecurity, passToken, serr := refreshPairedMaterial(ctx, s.HostSessionID, s.DeviceID)
			if serr != nil {
				return nil, fmt.Errorf("验证已通过，但读取会话密钥失败: %w", serr)
			}
			if ssecurity == "" {
				return nil, errors.New("验证已通过，但小米未返回 ssecurity；请重新登录再试")
			}
			out, perr := exchangeAndProbe(ctx, id, s, "", ssecurity, passToken)
			// 验证完成后会话使命结束：票据已密封进 HostVault。
			// 不关就会和 restoreCloudSession 一样把宿主的 4 个会话额度耗光
			// （用户实测 PLUGIN_RESOURCE_EXHAUSTED）。会话文件与内存密码同样清掉。
			_ = ctx.HTTPClose(s.HostSessionID)
			_ = ctx.FSRemove(sessionPath(id))
			challengeSecrets.delete(id)
			return out, perr
		}
		if detail != "" {
			lastDetail = detail
		}
	}
	if lastDetail == "" {
		lastDetail = "小米未说明原因"
	}
	return nil, fmt.Errorf("验证码未通过（小米返回：%s）", lastDetail)
}
