package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"homex.dev/homex/sdk/go/plugin"
)

type savedCloudSession struct {
	Version       int    `json:"version"`
	DeviceID      string `json:"device_id,omitempty"`
	Region        string `json:"region"`
	MaskedUser    string `json:"masked_user"`
	// Account 是**完整**账号（用户明确要求显示完整，不要加 *）。
	//
	// 它与 MaskedUser 并存而非替换：脱敏值仍用于日志与错误提示 —— 那些地方
	// 落地成文本后会被截图、进工单，写完整账号等于把身份散播到不可控副本里。
	// 展示面（配置页账号行）拿 Account，诊断面拿 MaskedUser。
	Account  string `json:"account,omitempty"`
	SavedAt  string `json:"saved_at,omitempty"`
	SecretRef     string `json:"secret_ref"`
	SecretVersion uint64 `json:"secret_version"`
	TicketsSealed bool   `json:"tickets_sealed"`
}
type cloudSecretMaterial struct {
	Ssecurity string `json:"ssecurity"`
	PassToken string `json:"pass_token,omitempty"`
	// Account 一并进保险库：它是身份，不该以明文躺在外面的 JSON 里。
	Account string `json:"account,omitempty"`
	// UserID 是小米账号数字 ID：step1 带 passToken 时必须同时带它，
	// 否则小米不认"同一个已登录账号"，下发不配对的材料（见 step1Ssecurity 注释）。
	UserID string `json:"user_id,omitempty"`
}

func exchangeAndProbe(ctx *plugin.Context, id string, s *loginSession, loc, ssecurity, passToken string) (json.RawMessage, error) {
	if loc != "" && !allowedVerifyURL(loc) && !strings.HasPrefix(loc, "https://sts.api.io.mi.com/") {
		return nil, errors.New("STS 地址不在可信范围")
	}
	// 钥匙必须在换票**之前**就非空。
	//
	// # 为什么这条守卫是必要的（用户实测 invalid signature 的直接成因之一）
	//
	// step1 "已有有效会话" 分支会同时给 (ssecurity, location)；若 ssecurity 缺席
	// （旧会话 cookie 里没有可吸收的 ssecurity、响应也没给），流程照样往下走，
	// 最后拿空串去 RC4 签名 —— 小米一律回 {"code":0,"message":"invalid signature"}，
	// 而这个错误看起来像"算法错了"，实际是"根本没拿到钥匙"。
	// 宁可在这里明确说"小米没下发会话密钥"，也不要产出一个误导人的签名错误。
	if strings.TrimSpace(ssecurity) == "" {
		return nil, errors.New("小米未下发会话密钥（ssecurity 为空）：请重新登录并完成验证")
	}
	if loc != "" {
		resp, err := ctx.HTTPSessionDo(plugin.HTTPSessionRequest{SessionID: s.HostSessionID, Method: "GET", URL: loc, TimeoutMS: 20000})
		if err != nil {
			return nil, fmt.Errorf("网络不确定: %w", err)
		}
		if resp.Status == 401 || resp.Status == 403 {
			host := ""
			if u, ue := url.Parse(loc); ue == nil {
				host = u.Hostname()
			}
			return nil, fmt.Errorf("认证票据被明确拒绝（host=%s，status=%d）", host, resp.Status)
		}
		if resp.Status < 200 || resp.Status >= 500 {
			return nil, fmt.Errorf("网络不确定: STS HTTP %d", resp.Status)
		}
	}
	sealed := 0
	hosts := []string{"account.xiaomi.com", "sts.api.io.mi.com", "api.io.mi.com"}
	for _, host := range hosts {
		r, e := ctx.HTTPCookiesSeal(plugin.HTTPCookiesRequest{SessionID: s.HostSessionID, Host: host})
		if e == nil {
			sealed += len(r.Sealed)
			ctx.Log(1, fmt.Sprintf("hx-mi sealed host=%s count=%d", host, len(r.Sealed)))
		} else {
			ctx.Log(2, fmt.Sprintf("hx-mi seal host=%s err=%v", host, e))
		}
	}
	if sealed == 0 {
		return nil, errors.New("STS 未下发可密封票据")
	}
	// 换票后把刚密封的票据立刻装回同一会话：部分小米跳转会把 cookie 落在
	// 中间 host/域上，宿主 jar 的域匹配可能没带到 api.io.mi.com，重新 attach
	// 按显式 host 写回后再探针（幂等，不影响已完成会话）。
	for _, host := range hosts {
		_, _ = ctx.HTTPCookiesAttach(plugin.HTTPCookiesRequest{SessionID: s.HostSessionID, Host: host})
	}
	// 探针必须走 Xiaomi xiaomiio RC4 协议；否则绝不 completed。
	body, err := apiPostRC4GuestDevice(ctx, s.HostSessionID, ssecurity, s.DeviceID, "https://api.io.mi.com/app/home/device_list", `{"getVirtualModel":true,"getHuamiDevices":1,"get_split_device":false,"support_smart_home":true}`)
	if err != nil {
		return nil, fmt.Errorf("%w（本次已密封票据 %d 条）", err, sealed)
	}
	var env map[string]json.RawMessage
	if json.Unmarshal(body, &env) != nil || env["result"] == nil {
		return nil, errors.New("设备探针响应不确定，保留票据但不完成登录")
	}
	s.State = "completed"
	s.Reason = ""
	s.Protocol.Challenge = nil
	challengeSecrets.delete(id)
	if err = saveLoginSession(ctx, id, *s); err != nil {
		return nil, err
	}
	secretBytes, _ := json.Marshal(cloudSecretMaterial{
		Ssecurity: ssecurity, PassToken: passToken,
		Account: s.Account, UserID: s.UserID,
	})
	secretInfo, err := ctx.VaultBlobSeal("xiaomi_session_material", secretBytes)
	if err != nil {
		return nil, err
	}
	saved := savedCloudSession{Version: 2, Region: s.Region, MaskedUser: s.MaskedUser,
		Account: s.Account, SavedAt: time.Now().UTC().Format(time.RFC3339),
		DeviceID: s.DeviceID, SecretRef: "xiaomi_session_material", SecretVersion: secretInfo.Version, TicketsSealed: true}
	b, _ := json.Marshal(saved)
	w := plugin.FSWriteRequest{Path: "cloud-session.json", QuotaKB: 4096}
	w.SetData(b)
	if err = ctx.FSWrite(w); err != nil {
		return nil, err
	}

	// ## 登录成功后必须登记到多账号库（否则"添加账号"加不进去）
	//
	// 这一句曾经缺失：登录把凭据写进 cloud-session.json 就返回了，account_store
	// 从头到尾没被写过。于是 accounts 工具永远返回空数组，前端 v2 分支不接管，
	// 用户看到的现象就是"登录成功，但账号列表里没有这个账号"。
	//
	// 失败不让登录变失败：凭据已经存好了，登记只是索引；如实记日志并继续，
	// 用户至少能正常使用单账号，而不是被一个索引写失败卡住。
	if regErr := registerAccountAfterLogin(ctx, s, secretBytes); regErr != nil {
		ctx.Log(2, "hx-mi account store register failed: "+regErr.Error())
	}

	return json.Marshal(map[string]any{"session_id": id, "state": "completed"})
}

func restoreCloudSession(ctx *plugin.Context) (string, error) {
	r, err := ctx.FSRead("cloud-session.json")
	if err != nil || !r.Found {
		return "no_session", err
	}
	b, err := r.Data()
	if err != nil {
		return "unreadable", err
	}
	var saved savedCloudSession
	if json.Unmarshal(b, &saved) != nil || !saved.TicketsSealed {
		return "unreadable", nil
	}
	// v1 旧格式含明文 ssecurity，受控迁移到 HostVault 后立即重写/删除明文。
	var material cloudSecretMaterial
	if saved.Version == 1 {
		var legacy struct {
			Ssecurity string `json:"ssecurity"`
		}
		if json.Unmarshal(b, &legacy) != nil || legacy.Ssecurity == "" {
			return "unreadable", nil
		}
		material.Ssecurity = legacy.Ssecurity
		info, e := ctx.VaultBlobSeal("xiaomi_session_material", []byte(`{"ssecurity":"`+legacy.Ssecurity+`"}`))
		if e != nil {
			return "unreadable", e
		}
		saved.Version = 2
		saved.SecretRef = "xiaomi_session_material"
		saved.SecretVersion = info.Version
		clean, _ := json.Marshal(saved)
		w := plugin.FSWriteRequest{Path: "cloud-session.json", QuotaKB: 4096}
		w.SetData(clean)
		if e = ctx.FSWrite(w); e != nil {
			return "unreadable", e
		}
	} else {
		secretBytes, _, e := ctx.VaultBlobOpen(saved.SecretRef)
		if e != nil {
			return "unreadable", e
		}
		if json.Unmarshal(secretBytes, &material) != nil || material.Ssecurity == "" {
			return "unreadable", nil
		}
	}
	hs, err := ctx.HTTPOpen(plugin.HTTPOpenRequest{})
	if err != nil {
		return "temporarily_unavailable", err
	}
	// ## 这次会话必须关（PLUGIN_RESOURCE_EXHAUSTED 的真正元凶）
	//
	// restoreCloudSession 是**只读探针**：开一个临时会话、贴票据、问一句
	// "还能用吗"、立刻就该关。早期实现六条 return 路径一条都没关，
	// 而 account 工具每次调用都走它 —— 前端一挂载就轮询 account/settings/regions，
	// 连点几次「退出重新登录」就凑满宿主 4 个会话上限，
	// 之后连 HTTPOpen 都失败（用户实测报错就停在这一句）。
	//
	// defer 而不是逐条 return 前手写：这条函数以后再加分支也不会再漏。
	defer func() { _ = ctx.HTTPClose(hs.SessionID) }()
	attached := 0
	for _, host := range []string{"account.xiaomi.com", "sts.api.io.mi.com", "api.io.mi.com"} {
		a, e := ctx.HTTPCookiesAttach(plugin.HTTPCookiesRequest{SessionID: hs.SessionID, Host: host})
		if e == nil {
			attached += len(a.Attached)
		}
	}
	if attached == 0 {
		return "auth_rejected", nil
	}
	// 探针必须带 deviceId：旧版对每个业务请求都带 deviceId/sdkVersion，
	// 新版这里曾传空 —— 表现为"凭据明明有效，界面却说不可用"（indeterminate）。
	body, probeErr := apiPostRC4GuestDevice(ctx, hs.SessionID, material.Ssecurity, saved.DeviceID,
		"https://api.io.mi.com/app/home/device_list",
		`{"getVirtualModel":true,"getHuamiDevices":1,"get_split_device":false,"support_smart_home":true}`)
	if probeErr != nil {
		// 如实记录探针失败原因：这个错误过去被吞成 indeterminate，
		// 用户只看到"账号不显示"，排障时无从下手。
		ctx.Log(2, "hx-mi restore probe failed: "+probeErr.Error())
		msg := probeErr.Error()
		if strings.Contains(msg, "明确拒绝") {
			return "auth_rejected", nil
		}
		if strings.Contains(msg, "网络不确定") {
			return "temporarily_unavailable", nil
		}
		return "indeterminate", nil
	}
	var env map[string]json.RawMessage
	if json.Unmarshal(body, &env) != nil || env["result"] == nil {
		head := string(body)
		if len(head) > 160 {
			head = head[:160]
		}
		ctx.Log(2, "hx-mi restore probe unexpected body: "+head)
		return "indeterminate", nil
	}
	return "available", nil
}
func rc4Crypt(key, data []byte) []byte {
	s := make([]byte, 256)
	for i := range s {
		s[i] = byte(i)
	}
	j := 0
	for i := 0; i < 256; i++ {
		j = (j + int(s[i]) + int(key[i%len(key)])) & 255
		s[i], s[j] = s[j], s[i]
	}
	i, j := 0, 0
	crypt := func(in []byte) []byte {
		out := make([]byte, len(in))
		for k, b := range in {
			i = (i + 1) & 255
			j = (j + int(s[i])) & 255
			s[i], s[j] = s[j], s[i]
			out[k] = b ^ s[(int(s[i])+int(s[j]))&255]
		}
		return out
	}
	crypt(make([]byte, 1024))
	return crypt(data)
}
func decodeSsecurity(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
// normalizeTrustedSTS 规范化并校验验证落地链接。
//
// 为什么不再直接比较 Hostname：实测用户从浏览器复制回来的地址可能带首尾
// 空白/引号（复制整个地址栏或从聊天工具粘贴），url.Parse 对这种输入会直接
// 报错，最终只得到一句“不可信”，无法定位是格式还是主机问题。这里先做
// 无损规范化，再按主机白名单判定；失败时把 scheme/host 回给调用方（不含
// query，避免票据进日志/响应）。
func normalizeTrustedSTS(raw string) (string, string) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.Trim(trimmed, "\"'")
	if trimmed == "" {
		return "", "链接为空"
	}
	u, e := url.Parse(trimmed)
	if e != nil {
		return "", "链接无法解析（需要完整地址，含 https://）"
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if scheme != "https" {
		return "", "链接必须使用 https（当前 scheme=" + u.Scheme + "）"
	}
	switch {
	case host == "sts.api.io.mi.com", host == "account.xiaomi.com":
		return u.String(), ""
	case strings.HasSuffix(host, ".xiaomi.com"):
		// 小米自有域下的其它验证页（区域/登录中转）；仍由 HTTPS + 域后缀限定。
		return u.String(), ""
	default:
		return "", "链接主机不在可信范围（当前 host=" + host + "）"
	}
}

func trustedSTS(raw string) bool {
	_, e := normalizeTrustedSTS(raw)
	return e == ""
}

// registerAccountAfterLogin 把刚完成的登录登记进多账号库。
//
// identity 用完整账号（account 优先，回落 masked）：upsert 以它为幂等键，
// 同一账号重复登录只更新不新增。secret 传会话材料（ssecurity/passToken），
// 由 accountStore 自己加密落盘。
func registerAccountAfterLogin(ctx *plugin.Context, s *loginSession, secret []byte) error {
	identity := s.Account
	if strings.TrimSpace(identity) == "" {
		identity = s.MaskedUser
	}
	if strings.TrimSpace(identity) == "" {
		return errors.New("缺少账号身份，无法登记")
	}
	store, err := openAccountStore(ctx)
	if err != nil {
		return err
	}
	region := s.Region
	if region == "" {
		region = "cn"
	}
	acc, created, err := store.upsert(identity, s.MaskedUser, region, secret)
	if err != nil {
		return err
	}
	verb := "updated"
	if created {
		verb = "created"
	}
	ctx.Log(1, "hx-mi account registered "+verb+" id="+acc.AccountID)
	return nil
}
