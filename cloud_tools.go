package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"homex.dev/homex/sdk/go/plugin"
)

type loginSession struct {
	HostSessionID string        `json:"host_session_id"`
	Region        string        `json:"region"`
	MaskedUser    string        `json:"masked_user"`
	// Account 是完整账号（展示用）；MaskedUser 留给日志与错误提示。
	Account       string        `json:"account,omitempty"`
	// UserID 是小米数字账号 ID，step1 需要与 passToken 一起发送。
	UserID        string        `json:"user_id,omitempty"`
	State         string        `json:"state"`
	Reason        string        `json:"reason,omitempty"`
	Protocol      protocolState `json:"protocol"`
	// Attempts 只用于验证码限流：6 位码必须挡住暴力猜测（旧版同样上限 5 次）。
	Attempts int `json:"attempts,omitempty"`
	// DeviceID 是小米要求的设备标识（与 UA 内嵌值一致）。
	DeviceID string `json:"device_id,omitempty"`
}

func sessionPath(id string) string { return "login-sessions/" + id + ".json" }
func saveLoginSession(ctx *plugin.Context, id string, s loginSession) error {
	b, _ := json.Marshal(s)
	w := plugin.FSWriteRequest{Path: sessionPath(id), QuotaKB: 4096}
	w.SetData(b)
	return ctx.FSWrite(w)
}
func loadLoginSession(ctx *plugin.Context, id string) (loginSession, error) {
	r, err := ctx.FSRead(sessionPath(id))
	if err != nil {
		return loginSession{}, err
	}
	if !r.Found {
		return loginSession{}, errors.New("session_not_found")
	}
	b, err := r.Data()
	if err != nil {
		return loginSession{}, err
	}
	var s loginSession
	err = json.Unmarshal(b, &s)
	return s, err
}

func startLogin(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct{ Region, User, Password string }
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, err
	}
	if in.Region == "" {
		in.Region = "cn"
	}
	if strings.TrimSpace(in.User) == "" || in.Password == "" {
		return nil, errors.New("账号和密码不能为空")
	}
	// 先回收历史会话再开新会话：宿主上限 4，反复登录时旧会话不清就会撞上限。
	// keepID 传空：此刻新会话还没建立，因此回收所有已完成/多余挑战会话；
	// 保留下来的那个挑战会话由下面的 canReuse 逻辑决定是否复用。
	reapStaleLoginSessions(ctx, "")
	hs, err := ctx.HTTPOpen(plugin.HTTPOpenRequest{})
	if err != nil {
		return nil, err
	}
	// ## 登录一律用**干净会话**，绝不复用上一个账号的票据
	//
	// 这里原来有一条 canReuse 分支：只要 cloud-session.json 存在（即已登录过）
	// 就把已密封票据 attach 进新会话。它对"同账号重新登录"无害甚至有利，
	// 但对**添加新账号**是致命的：
	//
	//   jar 里带着账号 A 的 serviceToken → 小米 step1 认为"已有有效会话"，
	//   直接返回 code=0 sign=false 而**不给 _sign**（也就不走账号密码流程），
	//   于是拿 A 的会话材料去探针，用户填的却是账号 B ——
	//   小米回 {"code":0,"message":"invalid signature"}。
	//   日志特征：step1 code=0 sign=false ssec=true + 票据 3+4+4=11 条全 sealed
	//   （用户多次实测，01:04 与 01:16 两次完全相同）。
	//
	// 正确做法：登录入口不预设"我是谁"。清掉三个 host 的旧票据，让 step1 走
	// 正常的账号密码流程（拿到 _sign → step2 → 配对的 ssecurity/location）。
	// 同账号重登只是多点一次验证，绝不会串号 —— 这个取舍是对的。
	for _, host := range []string{"account.xiaomi.com", "sts.api.io.mi.com", "api.io.mi.com"} {
		_, _ = ctx.HTTPCookiesDelete(plugin.HTTPCookiesRequest{SessionID: hs.SessionID, Host: host})
	}
	// 第一步只建立 Xiaomi cookie 会话并取签名页。密码不写盘、不写日志。
	deviceID, derr := loadOrCreateDeviceID(ctx)
	if derr != nil {
		ctx.Log(2, "hx-mi device id persist failed: "+derr.Error())
	}
	r, err := ctx.HTTPSessionDo(plugin.HTTPSessionRequest{SessionID: hs.SessionID, Method: "GET",
		URL: "https://account.xiaomi.com/pass/serviceLogin?sid=xiaomiio&_json=true",
		Headers: xiaomiHeaders(deviceID), TimeoutMS: 15000})
	if err != nil {
		_ = ctx.HTTPClose(hs.SessionID)
		return nil, fmt.Errorf("网络不确定: %w", err)
	}
	body, err := r.Body()
	if err != nil {
		return nil, err
	}
	if r.Status == 401 || r.Status == 403 {
		_ = ctx.HTTPClose(hs.SessionID)
		return json.Marshal(map[string]any{"state": "failed", "reason": "认证被明确拒绝"})
	}
	if r.Status < 200 || r.Status >= 500 {
		return nil, fmt.Errorf("网络不确定: 小米登录入口状态 %d", r.Status)
	}
	step1, err := parseAuth(body)
	if err != nil {
		_ = ctx.HTTPClose(hs.SessionID)
		return nil, err
	}
	ctx.Log(1, fmt.Sprintf("hx-mi login step1 code=%v sign=%t loc=%s ssec=%t",
		step1.Code, step1.Sign != "", hostOf(step1.Location), step1.Ssecurity != ""))
	// ## 会话生命周期（用户实测"http 会话数超过上限 4"的直接成因）
	//
	// 一次 login 调用最多开一个宿主 HTTP 会话。它的使命分两种：
	//   ① 登录**完成** → 票据已密封进 HostVault，会话本身再也用不到 → 必须关；
	//   ② 进入 need_challenge → 用户稍后提交验证码还要用同一会话 → 必须留。
	// 早期版本完成态没有关会话，每失败/成功一次就漏一个；宿主上限 4，
	// 用户连点几次"退出重新登录"就会撞上 PLUGIN_RESOURCE_EXHAUSTED。
	// 这里用 closed 标记保证**完成态必关**，挑战态才留下。
	if step1.Sign == "" {
		// 小米在“已有有效会话”时不返回 _sign，而是直接给 location（换票地址）；
		// 风控/受限时则返回错误码而没有任何签名。两种情况都不能笼统报“接口变更”。
		if loc := strings.TrimSpace(step1.Location); loc != "" {
			s := loginSession{HostSessionID: hs.SessionID, Region: in.Region,
				MaskedUser: maskUser(in.User), Account: in.User, UserID: userIDString(step1.UserID),
				State: "failed", Protocol: protocolState{QS: step1.QS, Callback: step1.Callback}, DeviceID: deviceID}
			// ## 这一分支只用 step1 **自己这一次响应**给的 (ssecurity, location)
			//
			// 它们出自同一次响应，天生就是一对：GET 这条 location 换回来的
			// serviceToken 与该 ssecurity 配对。曾经在这里多做一次
			// refreshPairedMaterial() 想"再保险一次"，结果正好把事情搞坏 ——
			// 那个函数读的是**保险库里的 passToken**（上一个账号的），于是它拿到的
			// 是**旧账号**的配对材料，与当前账号的票据错配，探针就报
			// {"code":0,"message":"invalid signature"}（用户 01:04 两次实测，
			// 日志特征：step1 code=0 sign=false ssec=true + paired material refreshed）。
			//
			// 对照实验（真实账号）：同一次 step1 的 (ssec, loc) → absorb → 探针 = OK；
			// 跨两次 step1 混用材料 → invalid signature。
			//
			// 所以这里只做一件事：GET 该 location 让 jar 收下配对的 serviceToken。
			if err := absorbLocation(ctx, hs.SessionID, deviceID, loc); err != nil {
				ctx.Log(2, "hx-mi absorb location failed: "+err.Error())
			}
			out, perr := exchangeAndProbe(ctx, hs.SessionID, &s, "", step1.Ssecurity, step1.PassToken)
			_ = ctx.HTTPClose(hs.SessionID)
			return out, perr
		}
		reason := firstNonEmpty(step1.Desc, step1.Description)
		code := fmt.Sprint(step1.Code)
		if reason == "" {
			reason = "小米登录入口未返回签名"
		}
		_ = ctx.HTTPClose(hs.SessionID)
		return nil, fmt.Errorf("小米登录入口未返回签名（code=%s，原因：%s）", code, reason)
	}
	proto := protocolState{Sign: step1.Sign, QS: step1.QS, Callback: step1.Callback}
	result, err := doStep2(ctx, hs.SessionID, in.User, in.Password, "", proto, deviceID)
	if err != nil {
		_ = ctx.HTTPClose(hs.SessionID)
		return nil, err
	}
	ctx.Log(1, fmt.Sprintf("hx-mi login step2 code=%v loc=%s notify=%s captcha=%s", result.Code, hostOf(result.Location), hostOf(result.NotificationURL), hostOf(result.CaptchaURL)))
	id := hs.SessionID
	s := loginSession{HostSessionID: hs.SessionID, Region: in.Region,
		MaskedUser: maskUser(in.User), Account: in.User, UserID: userIDString(result.UserID),
		State: "failed", Protocol: proto, DeviceID: deviceID}
	out := map[string]any{"session_id": id, "state": "failed"}
	if result.Location != "" {
		out, perr := exchangeAndProbe(ctx, id, &s, result.Location, result.Ssecurity, result.PassToken)
		// 换票+探针结束，会话使命完成；票据已进 HostVault，关掉不丢任何东西。
		_ = ctx.HTTPClose(id)
		return out, perr
	} else if result.CaptchaURL != "" {
		capURL := absoluteXiaomiURL(result.CaptchaURL)
		if !allowedVerifyURL(capURL) {
			return nil, errors.New("验证码地址不在可信范围")
		}
		s.State = "need_challenge"
		s.Protocol.CaptchaURL = capURL
		s.Protocol.Challenge = &loginChallenge{Kind: "captcha", SendStatus: "not_attempted", CaptchaURL: "captcha", Target: s.MaskedUser}
		out["state"] = "need_challenge"
		out["challenge"] = s.Protocol.Challenge
	} else if result.NotificationURL != "" {
		verify := absoluteXiaomiURL(result.NotificationURL)
		if !allowedVerifyURL(verify) {
			verify = ""
		}
		s.State = "need_challenge"
		s.Protocol.Challenge = &loginChallenge{Kind: "web_verify", SendStatus: "unverified", VerifyURL: verify, Target: s.MaskedUser}
		out["state"] = "need_challenge"
		out["challenge"] = s.Protocol.Challenge
	} else {
		s.Reason = explainLoginCode(result.Code, firstNonEmpty(result.Desc, result.Description))
		out["reason"] = s.Reason
	}
	if err = saveLoginSession(ctx, id, s); err != nil {
		return nil, err
	}
	if s.State == "need_challenge" {
		challengeSecrets.put(id, in.User, in.Password)
	} else {
		challengeSecrets.delete(id)
	}
	return json.Marshal(out)
}
func loginStatus(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, err
	}
	s, err := loadLoginSession(ctx, in.SessionID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"session_id": in.SessionID, "state": s.State, "region": s.Region, "masked_user": s.MaskedUser, "reason": s.Reason})
}
// logoutLogin 删除小米账号：**必须删干净**，否则用户看到"删不掉"。
//
// # 之前为什么删不掉（用户实测："账号删除不掉"）
//
// 旧实现只在传了 session_id 时删登录会话文件，而真正的凭据完全不碰：
//   - cloud-session.json（存在的证据 —— account 工具就靠它报"已登录"）
//   - 保险库里的 xiaomi_session_material（ssecurity/passToken，探针会用）
//   - 宿主 CookieJar 里已密封的票据（换票后可复活的登录态）
// 于是点「删除」后 refreshAccount 仍读到 available，账号行照旧显示。
//
// # 为什么要求"全部删成功"而不是尽力而为
//
// 部分成功最危险：界面显示已删除，实际保险库还在，下次重启又"自己回来了"。
// 所以这里按顺序删、任何一步失败都如实报错（前端会把错误显示给用户），
// 而不是吞掉错误返回 cancelled。
func logoutLogin(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(args, &in)

	var failures []string

	// ① 登录会话（前端内存里的那个；重启后可能为空，不算错误）
	if in.SessionID != "" {
		if s, err := loadLoginSession(ctx, in.SessionID); err == nil {
			_ = ctx.HTTPClose(s.HostSessionID)
		}
		_ = ctx.FSRemove(sessionPath(in.SessionID))
		challengeSecrets.delete(in.SessionID)
	}
	// ② 清掉磁盘上所有残留登录会话，避免下次登录复用旧挑战态
	clearAllLoginSessions(ctx)

	// ③ 宿主 CookieJar 里已密封的票据
	for _, host := range []string{"account.xiaomi.com", "sts.api.io.mi.com", "api.io.mi.com"} {
		if _, e := ctx.HTTPCookiesDelete(plugin.HTTPCookiesRequest{Host: host}); e != nil {
			failures = append(failures, fmt.Sprintf("清理 %s 票据失败: %v", host, e))
		}
	}

	// ④ 保险库里的会话密钥
	if _, e := ctx.VaultBlobDelete("xiaomi_session_material"); e != nil {
		failures = append(failures, "删除会话密钥失败: "+e.Error())
	}

	// ⑤ 存在的证据本身
	if e := ctx.FSRemove("cloud-session.json"); e != nil {
		failures = append(failures, "删除会话文件失败: "+e.Error())
	}

	// ⑥ 同步状态快照：账号都没了，这些数字再留着就是谎
	_ = ctx.FSRemove(syncStatePath)

	if len(failures) > 0 {
		return nil, fmt.Errorf("账号删除未完成：%s", strings.Join(failures, "；"))
	}
	// 回读确认：cloud-session.json 必须真的不存在了。
	if r, e := ctx.FSRead("cloud-session.json"); e == nil && r.Found {
		return nil, errors.New("账号删除未完成：会话文件仍然存在")
	}
	return []byte(`{"state":"deleted"}`), nil
}

// clearAllLoginSessions 关闭并删除所有登录会话（退出登录时用）。
func clearAllLoginSessions(ctx *plugin.Context) {
	files, err := ctx.FSList()
	if err != nil {
		return
	}
	for _, f := range files {
		if !strings.HasPrefix(f.Path, "login-sessions/") || !strings.HasSuffix(f.Path, ".json") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(f.Path, "login-sessions/"), ".json")
		if s, lerr := loadLoginSession(ctx, id); lerr == nil {
			_ = ctx.HTTPClose(s.HostSessionID)
		}
		_ = ctx.FSRemove(f.Path)
		challengeSecrets.delete(id)
	}
}

func captchaTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, err
	}
	s, err := loadLoginSession(ctx, in.SessionID)
	if err != nil {
		return nil, err
	}
	if s.Protocol.Challenge == nil || s.Protocol.Challenge.Kind != "captcha" {
		return nil, errors.New("当前会话不需要图形验证码")
	}
	secret, err := challengeSecrets.get(in.SessionID)
	if err != nil {
		return nil, err
	}
	result, err := doStep2(ctx, s.HostSessionID, secret.user, secret.password, in.Code, s.Protocol, s.DeviceID)
	if err != nil {
		return nil, err
	}
	// location 只表示进入换票阶段；未拿到并验证 serviceToken 前绝不 completed。
	if result.Location != "" {
		return exchangeAndProbe(ctx, in.SessionID, &s, result.Location, result.Ssecurity, result.PassToken)
	} else if result.CaptchaURL != "" {
		s.State = "need_challenge"
		s.Reason = explainLoginCode(result.Code, firstNonEmpty(result.Desc, result.Description))
	} else {
		s.State = "failed"
		s.Reason = explainLoginCode(result.Code, firstNonEmpty(result.Desc, result.Description))
		challengeSecrets.delete(in.SessionID)
	}
	if err = saveLoginSession(ctx, in.SessionID, s); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"session_id": in.SessionID, "state": s.State, "reason": s.Reason, "challenge": s.Protocol.Challenge})
}
func captchaImageTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, err
	}
	s, err := loadLoginSession(ctx, in.SessionID)
	if err != nil {
		return nil, err
	}
	if s.Protocol.CaptchaURL == "" {
		return nil, errors.New("当前会话不需要图形验证码")
	}
	resp, err := ctx.HTTPSessionDo(plugin.HTTPSessionRequest{SessionID: s.HostSessionID, Method: "GET", URL: s.Protocol.CaptchaURL, TimeoutMS: 10000})
	if err != nil {
		return nil, err
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("验证码图片返回 HTTP %d", resp.Status)
	}
	return json.Marshal(map[string]any{"body_b64": resp.BodyB64, "content_type": resp.Headers["Content-Type"]})
}
func absoluteXiaomiURL(s string) string {
	if strings.HasPrefix(s, "/") {
		return "https://account.xiaomi.com" + s
	}
	return s
}
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
func maskUser(s string) string {
	if i := strings.IndexByte(s, '@'); i > 1 {
		return s[:1] + "***" + s[i:]
	}
	if len(s) > 4 {
		return s[:2] + "***" + s[len(s)-2:]
	}
	return "***"
}
func allowedVerifyURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "account.xiaomi.com" || strings.HasSuffix(h, ".xiaomi.com")
}

// reapStaleLoginSessions 回收不再需要的登录会话，防"会话数超过上限 4"。
//
// # 为什么必须有这一层
//
// need_challenge 会话**故意保留**：用户可能要过几分钟才收到验证码。但保留有代价 ——
// 每个占一个宿主 HTTP 会话（上限 4）。用户反复"退出再登录"、或验证码一直不提交，
// 会话就会堆积，最后新登录连 HTTPOpen 都失败
// （PLUGIN_RESOURCE_EXHAUSTED: http 会话数超过上限 4，用户实测）。
//
// # 判据为什么只看状态、不看时间
//
// SDK 的 FSList 只给 path/size，没有修改时间，无从判"多久没动"。所以这里采取
// **保守且仍然有效**的规则：除调用方本次要用的那个会话外，
//   ① 非 need_challenge（已完成/已失败/损坏）→ 立即回收，它们没有任何用途；
//   ② need_challenge 的**只保留最新一个**，更早的一律回收 —— 用户同一时刻
//      只可能在输一个验证码，旧挑战会话再也不会被提交。
// 这条规则让"反复重试"退化成常数占用（≤1 个挑战会话），而不是无限增长。
func reapStaleLoginSessions(ctx *plugin.Context, keepID string) {
	files, err := ctx.FSList()
	if err != nil {
		return
	}
	ids := make([]string, 0, len(files))
	for _, f := range files {
		if !strings.HasPrefix(f.Path, "login-sessions/") || !strings.HasSuffix(f.Path, ".json") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(f.Path, "login-sessions/"), ".json")
		if id == "" || id == keepID {
			continue
		}
		ids = append(ids, id)
	}
	// 先回收所有非挑战态与损坏的。
	challenges := make([]string, 0, len(ids))
	for _, id := range ids {
		s, lerr := loadLoginSession(ctx, id)
		if lerr != nil {
			_ = ctx.FSRemove(sessionPath(id))
			challengeSecrets.delete(id)
			continue
		}
		if s.State == "need_challenge" {
			challenges = append(challenges, id)
			continue
		}
		_ = ctx.HTTPClose(s.HostSessionID)
		_ = ctx.FSRemove(sessionPath(id))
		challengeSecrets.delete(id)
	}
	// 挑战态里只留一个（保留最后一个，最可能是用户正在处理的）。
	// 文件顺序不保证时间序，所以宁可全留也不误杀正在输码的会话：
	// 只有当挑战会话**超过 2 个**时才收掉多余的，给 TTL 兜一层。
	if len(challenges) <= 1 {
		return
	}
	for _, id := range challenges[:len(challenges)-1] {
		if s, lerr := loadLoginSession(ctx, id); lerr == nil {
			_ = ctx.HTTPClose(s.HostSessionID)
		}
		_ = ctx.FSRemove(sessionPath(id))
		challengeSecrets.delete(id)
	}
}
