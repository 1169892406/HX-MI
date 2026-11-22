// HX-MI · 扫码登录的宿主适配层（HTTPSession 版）。
//
// qrlogin 包是平台无关的（传输/存储抽象成接口）；这里把它的 IO 全部接到
// 宿主托管会话（HTTPOpen / HTTPSessionDo / HTTPCookiesSeal）——与密码登录
// （cloud_protocol.go / token_exchange.go）同一条通道。
//
// ## 为什么不能走裸 ctx.HTTPRequest（2026-11-22，扫码 70036 排障结论）
//
// 裸出站在宿主沙箱里有四道断点，每一道都足以让扫码流程走不完：
//  1. 请求头白名单把 Cookie 列为插件禁设头（netgate isHopByHopOrSensitive），
//     bootstrap/miLogin 的 `sdkVersion; deviceId; passToken=` 全部被静默丢弃；
//  2. 响应 Set-Cookie 被 plainHeaders 剥掉——插件永远"看不见" serviceToken；
//  3. 重定向一律不跟随（CheckRedirect → ErrUseLastResponse），clientSign 回调的
//     302→STS 跳转链走不完；
//  4. 长轮询地址是动态子域（ak.lp.account.xiaomi.com 等），manifest 精确域名
//     匹配拦下（已补 `*.account.xiaomi.com`）。
//
// HTTPSession 三者全通：真 cookie jar（Set-Cookie 归宿主、跨请求自动回带）、
// 跟随重定向≤5 跳、允许显式 Cookie 头。换票成功后 CookiesSeal 把票据密封进
// HostVault（与密码登录同键位），探针 + 账号登记复用 exchangeAndProbe。
package main

import (
	crand "crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"qrlogin"

	"homex.dev/homex/sdk/go/plugin"
)

// ---------- 运行时：ctx 代理 + 宿主会话 + 存储 ----------

// qrRuntime 实现 qrlogin.Transport / qrlogin.Store，并承载 Exchange。
// wasm 实例内存跨调用保留；每次工具调用换新的 ctx，用 setContext 替换。
type qrRuntime struct {
	mu   sync.Mutex
	ctx  *plugin.Context
	sess string // 宿主会话 ID（懒开；cookie jar 在宿主侧，跨请求收发 Set-Cookie）
}

func (r *qrRuntime) setContext(ctx *plugin.Context) {
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
}

func (r *qrRuntime) current() *plugin.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ctx
}

// sessionGone 识别"宿主会话不存在或已关闭"（实例重启/宿主重启后内存态丢失）。
// 命中时 Do/Exchange 会重开会话再试一次；宿主错误文本与 pluginres/httpsession.go
// 的 errPermission 文案一致。
func sessionGone(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "会话不存在")
}

// ensureSession 返回可用的宿主会话 ID（无则开一个）。
func (r *qrRuntime) ensureSession(ctx *plugin.Context) (string, error) {
	r.mu.Lock()
	sess := r.sess
	r.mu.Unlock()
	if sess != "" {
		return sess, nil
	}
	hs, err := ctx.HTTPOpen(plugin.HTTPOpenRequest{})
	if err != nil {
		return "", fmt.Errorf("开宿主会话失败: %w", err)
	}
	r.mu.Lock()
	r.sess = hs.SessionID
	r.mu.Unlock()
	return hs.SessionID, nil
}

// resetSession 关掉旧会话并开一个全新的（cookie 干净）。
//
// 每次新的扫码引导都必须用干净会话：旧扫描残留的 jar cookie（如半途拿到的
// serviceToken）会让 serviceLogin 误判"已登录"，返回不带 QR 参数的 location，
// 生成一张扫不出登录确认的废码——控制台（无 jar 的 net/http）没有这个问题，
// 插件里必须主动重置。
func (r *qrRuntime) resetSession(ctx *plugin.Context) error {
	r.mu.Lock()
	old := r.sess
	r.sess = ""
	r.mu.Unlock()
	if old != "" {
		_ = ctx.HTTPClose(old)
	}
	_, err := r.ensureSession(ctx)
	return err
}

// closeSession 丢弃会话（换票完成/失败后收尾，避免挤占宿主 4 会话上限）。
func (r *qrRuntime) closeSession() {
	r.mu.Lock()
	old := r.sess
	r.sess = ""
	r.mu.Unlock()
	if old != "" {
		if ctx := r.current(); ctx != nil {
			_ = ctx.HTTPClose(old)
		}
	}
}

// ---------- qrlogin.Transport ----------

func (r *qrRuntime) Do(method, u string, headers map[string]string, timeout time.Duration) (qrlogin.Response, error) {
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	// 压到执行预算内（manifest execution_timeout_ms=30000；宿主单请求上限 30s）
	ms := int(timeout.Milliseconds())
	if ms > 25000 {
		ms = 25000
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		ctx := r.current()
		if ctx == nil {
			return qrlogin.Response{}, fmt.Errorf("qr 传输缺少宿主上下文")
		}
		sess, err := r.ensureSession(ctx)
		if err != nil {
			return qrlogin.Response{}, err
		}
		resp, err := ctx.HTTPSessionDo(plugin.HTTPSessionRequest{
			SessionID: sess, Method: method, URL: u,
			Headers: headers, TimeoutMS: ms,
		})
		if err != nil {
			// 宿主重启/会话被回收：重开一次再试（jar 丢失——lp 长轮询按 ticket 寻址、
			// bootstrap 自带 Cookie 头，都不依赖旧 cookie，重开是安全的）。
			if sessionGone(err) && attempt == 0 {
				r.mu.Lock()
				r.sess = ""
				r.mu.Unlock()
				lastErr = err
				continue
			}
			return qrlogin.Response{}, err
		}
		body, err := resp.Body()
		if err != nil {
			return qrlogin.Response{}, err
		}
		return qrlogin.Response{Status: resp.Status, Headers: resp.Headers, Body: body}, nil
	}
	return qrlogin.Response{}, lastErr
}

// ---------- qrlogin.Store ----------

const qrStorePrefix = "qrlogin/"

func (r *qrRuntime) Get(key string) (string, bool, error) {
	ctx := r.current()
	if ctx == nil {
		return "", false, fmt.Errorf("qr 存储缺少宿主上下文")
	}
	resp, err := ctx.StorageGet(qrStorePrefix + key)
	if err != nil {
		return "", false, err
	}
	return resp.Value, resp.Found, nil
}

func (r *qrRuntime) Put(key, value string) error {
	ctx := r.current()
	if ctx == nil {
		return fmt.Errorf("qr 存储缺少宿主上下文")
	}
	return ctx.StoragePut(qrStorePrefix+key, value)
}

// ---------- Exchange：扫码确认后的换票（宿主托管 cookie 专属路径） ----------

// qrClientSign 与 qrlogin.clientSign 同一算法（passport 规矩：
// location 必须附 base64(sha1("nonce="+nonce+"&"+ssecurity))，否则不种 cookie）。
func qrClientSign(nonce, ssecurity string) string {
	sum := sha1.Sum([]byte("nonce=" + nonce + "&" + ssecurity))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// withClientSign 给回调地址补上 clientSign 参数。
func withClientSign(loc, nonce, ssecurity string) string {
	sep := "&"
	if !strings.Contains(loc, "?") {
		sep = "?"
	}
	return loc + sep + "clientSign=" + url.QueryEscape(qrClientSign(nonce, ssecurity))
}

// exchange 扫码确认（lp 返回 code=0）后的换票整段：
//
//  1. clientSign 回调（会话内 GET，宿主跟随 302 链把 serviceToken 收进 jar）；
//  2. CookiesSeal 三主机（与密码登录同键位 xiaomi_service / xiaomi_account）；
//  3. exchangeAndProbe 复用：RC4 探针 + VaultBlob + 多账号登记；
//  4. 失败且有 passToken 时，serviceLogin 免密重取一次（与 qrlogin.miLogin 同语义）。
//
// 成功返回 userID；错误最终汇入扫码状态的 error 文案。
func (r *qrRuntime) exchange(out map[string]any) (string, error) {
	ctx := r.current()
	if ctx == nil {
		return "", fmt.Errorf("qr 换票缺少宿主上下文")
	}
	loc := qrStr(out["location"])
	nonce := qrStr(out["nonce"])
	ssec := qrStr(out["ssecurity"])
	userID := qrStr(out["userId"])
	passToken := qrStr(out["passToken"])
	if loc == "" || nonce == "" || ssec == "" {
		return "", fmt.Errorf("扫码结果缺少 location/nonce/ssecurity")
	}
	deviceID, err := r.deviceID()
	if err != nil {
		return "", err
	}

	// 账号身份：扫码没有手机号/邮箱，用数字 userId（账号列表按它展示与去重）。
	identity := userID
	if identity == "" {
		identity = "qr-" + deviceID
	}
	s := &loginSession{
		HostSessionID: "", Region: "cn",
		MaskedUser: identity, Account: identity,
		UserID: userID, State: "failed", DeviceID: deviceID,
	}

	sess, err := r.ensureSession(ctx)
	if err != nil {
		return "", err
	}
	s.HostSessionID = sess

	/*
	 * 换票顺序（2026-11-22 最终定稿，依据服务器实测）：
	 *
	 *   ① **passToken 免密 → xiaomiio**，且必须在**全新会话**里做。
	 *      实测（干净 jar）：serviceLogin(passToken+userId) 返回完整材料
	 *        nonce=4913608617542827008  ssecurity=0362N/...  location=sts.api.io.mi.com
	 *      GET location → Set-Cookie: serviceToken/userId/cUserId ✅
	 *      随后用同一会话、同一 ssecurity 做 RC4 探针 —— 与密码登录同一条链路。
	 *   ② passToken → micoapi：等价退路（域不同而已）。
	 *   ③ 扫码票据直接换（lp 的 location+nonce+ssecurity）：**最后**才试。
	 *
	 * 为什么 ③ 排最后（用户实测）：
	 *   `扫码票据换票（host=sts.api.io.mi.com）：小米业务接口拒绝（code=0，
	 *    说明：invalid signature）（本次已密封票据 3 条）`
	 *   —— 票据密封成功（STS 认了 clientSign），但 lp 给的 ssecurity 与 STS 最终
	 *   下发的 serviceToken **不配对**，RC4 签名过不了。而 ① 的 ssecurity 是
	 *   passport 在同一个响应里与 location 一起给的，配对天然成立。
	 *
	 * 关键点：passToken 换票**不能复用扫码会话**。复用会让 passport 把请求当成
	 * "已有会话"，只回 code=0 + location + psecurity（**没有 nonce/ssecurity**），
	 * 于是没法签名 —— 这正是用户看到的
	 *   `serviceLogin 响应缺少 location/nonce/ssecurity（… 有nonce=false …）`。
	 */
	var errs []string

	// ①② passToken 免密（新会话；每次新 deviceId，与 console 一致）
	for _, sid := range []string{"xiaomiio", "micoapi"} {
		if passToken == "" {
			errs = append(errs, "passToken 缺失，跳过 "+sid)
			continue
		}
		freshID := qrDeviceID()
		sess2, loc2, ssec2, pt2, ferr := r.serviceLoginWithPassToken(ctx, sid, freshID, userID, passToken)
		if ferr != nil {
			errs = append(errs, fmt.Sprintf("%s 取票：%v", sid, ferr))
			if sess2 != "" {
				_ = ctx.HTTPClose(sess2)
			}
			continue
		}
		s.State = "failed"
		// 换票、密封、探针全部留在**取票那个会话**里（ssecurity ↔ serviceToken 配对）
		s.HostSessionID = sess2
		/*
		 * ★★ 2026-11-22（`invalid signature` 的最终根因）：
		 *
		 * 这一行**必须**写。上面取票用的是 `freshID`（新 deviceId），因此 passport
		 * 下发的 serviceToken 是**绑定 freshID** 的；而 `s.DeviceID` 在 exchange
		 * 开头被设成了落盘的旧 deviceId。RC4 探针会用 s.DeviceID 拼
		 * `Cookie: deviceId=…` 与 UA，于是服务端看到"票据绑 A、请求自称 B"，
		 * 直接回 `{"code":0,"message":"invalid signature"}`（小米用这个错误表示
		 * deviceId 与票据不匹配，容易误判成签名算法问题）。
		 *
		 * 用户实测错误正是：`xiaomiio 换票：小米业务接口拒绝（code=0，
		 * 说明：invalid signature）（本次已密封票据 3 条）` —— 票据拿得到，
		 * 只是 deviceId 对不上。
		 */
		s.DeviceID = freshID
		_, e2 := exchangeAndProbe(ctx, sess2, s, loc2, ssec2, pt2)
		_ = ctx.HTTPClose(sess2)
		if e2 == nil {
			r.finish(userID, pt2, freshID)
			return userID, nil
		}
		errs = append(errs, fmt.Sprintf("%s 换票：%v", sid, e2))
	}

	// ③ 扫码票据直接换（兜底）
	if loc != "" && nonce != "" && ssec != "" {
		s.State = "failed"
		s.HostSessionID = sess
		if _, err := exchangeAndProbe(ctx, sess, s, withClientSign(loc, nonce, ssec), ssec, passToken); err == nil {
			r.finish(userID, passToken, deviceID)
			return userID, nil
		} else {
			host := ""
			if u, ue := url.Parse(loc); ue == nil {
				host = u.Hostname()
			}
			errs = append(errs, fmt.Sprintf("扫码票据换票（host=%s）：%v", host, err))
		}
	}

	r.closeSession()
	return "", fmt.Errorf("%s", strings.Join(errs, "；"))
}

// finish 换票成功的收尾：稳定设备标识与会话材料落盘（下个实例重启后
// bootstrap/续期仍用同一 deviceId，passport 才认得这台"设备"），让 Client
// 内存态与落盘一致，并关闭宿主会话。
func (r *qrRuntime) finish(userID, passToken, deviceID string) {
	tok := map[string]any{"deviceId": deviceID, "saveTime": time.Now().UnixMilli()}
	if userID != "" {
		tok["userId"] = userID
	}
	if passToken != "" {
		tok["passToken"] = passToken
	}
	if b, err := json.Marshal(tok); err == nil {
		_ = r.Put(".mi.token", string(b))
	}
	qrMuTryLoad()
	r.closeSession()
}

// serviceLoginWithPassToken 用 passToken 免密换新的一组 (location, ssecurity)。
// Cookie 头显式携带（HTTPSession 允许插件设置 Cookie；jar 里的同名 cookie 会并排附加）。
// serviceLoginWithPassToken 用 passToken 免密换取新的 (location, ssecurity, passToken)。
//
// # 必须用**全新会话**（2026-11-22 实测，用户错误文案里"缺少 nonce/ssecurity"的成因）
//
// 复用扫码那个会话时，jar 里已经有 bootstrap 留下的 cookie，passport 会把请求
// 当成"已有会话"处理，只回一个**简化响应**：
//
//	code=0 desc="成功" 有location=true 有nonce=false 有ssecurity=false
//	字段=[pwd child code securityStatus userId **psecurity** location desc ...]
//
// 没有 ssecurity 就没法做 RC4 签名，换票必然失败。而在**干净会话**里同一个请求
// 返回的是完整材料（服务器实测）：
//
//	code=0  nonce=4913608617542827008  ssecurity=0362N/fIMgiRpc+8UJQxjQ==
//	location=https://sts.api.io.mi.com/sts?...
//	GET location → Set-Cookie: serviceToken / userId / cUserId
//
// 密码登录之所以一直正常，正是因为它每次 `login` 都新开一个宿主会话。
func (r *qrRuntime) serviceLoginWithPassToken(ctx *plugin.Context, sid, deviceID, userID, passToken string) (string, string, string, string, error) {
	if sid == "" {
		sid = "xiaomiio"
	}
	// 全新会话：干净 jar ⇒ passport 返回完整 nonce/ssecurity。
	hs, oerr := ctx.HTTPOpen(plugin.HTTPOpenRequest{})
	if oerr != nil {
		return "", "", "", "", fmt.Errorf("开换票会话失败: %w", oerr)
	}
	// ⚠ 不在这里关闭：调用方还要用**同一个会话**去 GET location（让 serviceToken
	// 落进这个干净 jar）并做 RC4 探针。换票跨会话 ⇒ ssecurity 与 serviceToken
	// 不配对 ⇒ `invalid signature`（用户实测过）。
	sess := hs.SessionID

	cookie := fmt.Sprintf("sdkVersion=3.9; deviceId=%s; passToken=%s", deviceID, passToken)
	if userID != "" {
		cookie = fmt.Sprintf("sdkVersion=3.9; deviceId=%s; userId=%s; passToken=%s", deviceID, userID, passToken)
	}
	resp, err := ctx.HTTPSessionDo(plugin.HTTPSessionRequest{
		SessionID: sess, Method: "GET",
		URL: "https://account.xiaomi.com/pass/serviceLogin?sid=" + sid + "&_json=true",
		Headers: map[string]string{
			"Cookie":     cookie,
			"User-Agent": qrlogin.RandomUA(),
		},
		TimeoutMS: 20000,
	})
	if err != nil {
		return sess, "", "", "", fmt.Errorf("serviceLogin: %w", err)
	}
	body, err := resp.Body()
	if err != nil {
		return sess, "", "", "", err
	}
	raw := strings.TrimSpace(strings.ReplaceAll(string(body), "&&&START&&&", ""))
	var d map[string]any
	// UseNumber：passport 的 nonce 是 19 位整数，float64 存不下（53 位尾数），
	// 一旦被浮点化就会给 clientSign 喂错值 ⇒ invalid signature。见 qrlogin/http.go 注释。
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&d) != nil {
		return sess, "", "", "", fmt.Errorf("serviceLogin 返回异常: %.120s", raw)
	}
	if code := qrInt(d["code"]); code != 0 {
		return sess, "", "", "", fmt.Errorf("serviceLogin code=%d %s", code, qrStr(d["description"]))
	}
	loc := qrStr(d["location"])
	nonce := qrStr(d["nonce"])
	ssec := qrStr(d["ssecurity"])
	if loc == "" || nonce == "" || ssec == "" {
		// 把**实际拿到了什么**写进错误：只有"缺少 xxx"无法判断是 passport 的
		// 语义变化，还是我们请求参数不对（2026-11-22 排障吃过这个亏）。
		keys := make([]string, 0, len(d))
		for k := range d {
			keys = append(keys, k)
		}
		return sess, "", "", "", fmt.Errorf(
			"serviceLogin 响应缺少 location/nonce/ssecurity（code=%d desc=%q 有location=%v 有nonce=%v 有ssecurity=%v 字段=%v）",
			qrInt(d["code"]), qrStr(d["description"]), loc != "", nonce != "", ssec != "", keys)
	}
	if pt := qrStr(d["passToken"]); pt != "" {
		passToken = pt // 服务端轮换时采用新值
	}
	return sess, withClientSign(loc, nonce, ssec), ssec, passToken, nil
}

// deviceID 取稳定的设备标识（落盘复用；首次生成 16 位大写 hex）。
// bootstrap 的 Cookie 与 RC4 探针头共用它，保证 passport 看到同一台"设备"。
func (r *qrRuntime) deviceID() (string, error) {
	raw, found, err := r.Get(".mi.token")
	if err == nil && found {
		var tok map[string]any
		if json.Unmarshal([]byte(raw), &tok) == nil {
			if v := qrStr(tok["deviceId"]); v != "" {
				return v, nil
			}
		}
	}
	id := qrDeviceID()
	tok := map[string]any{"deviceId": id, "saveTime": time.Now().UnixMilli()}
	b, _ := json.Marshal(tok)
	if err := r.Put(".mi.token", string(b)); err != nil {
		return "", fmt.Errorf("设备标识落盘失败: %w", err)
	}
	return id, nil
}

// ---------- 会话管理与工具实现（call() switch 分发） ----------

var (
	qrMu     sync.Mutex
	qrClient *qrlogin.Client
	qrRT     *qrRuntime
)

// seedDeviceID 在首次创建 Client 前确保存储里有稳定 deviceId：
// qrlogin 的 bootstrap 从已加载 token 读 deviceId，空则随机——插件实例一重启
// 就换一台"新设备"，正是控制台（auth.json 常驻）与插件行为分叉的来源之一。
func seedDeviceID(ctx *plugin.Context) {
	resp, err := ctx.StorageGet(qrStorePrefix + ".mi.token")
	if err == nil && resp.Found {
		var tok map[string]any
		if json.Unmarshal([]byte(resp.Value), &tok) == nil && qrStr(tok["deviceId"]) != "" {
			return
		}
	}
	b, _ := json.Marshal(map[string]any{"deviceId": qrDeviceID(), "saveTime": time.Now().UnixMilli()})
	_ = ctx.StoragePut(qrStorePrefix+".mi.token", string(b))
}

// qrClientFor 用当前调用的 ctx 组装/复用扫码会话。
// Client 状态机在 qrlogin.Client 内跨调用保留；IO 代理（qrRuntime）按次换 ctx。
func qrClientFor(ctx *plugin.Context) *qrlogin.Client {
	qrMu.Lock()
	defer qrMu.Unlock()
	if qrClient == nil {
		seedDeviceID(ctx)
		qrRT = &qrRuntime{}
		qrRT.setContext(ctx)
		qrClient = qrlogin.New(qrlogin.Options{
			/*
			 * ★★ 2026-11-22（扫码 70036 根因）：
			 *
			 * 这里必须写 **".mi.token"**，不能写 "qrlogin/.mi.token"。
			 * qrRuntime.Get/Put 自己会加 qrStorePrefix（"qrlogin/"）前缀，而
			 * qrlogin.Client 把 TokenPath 原样交给 store —— 于是写成
			 * "qrlogin/.mi.token" 时实际落盘键是 `qrlogin/qrlogin/.mi.token`：
			 *   · bootstrap 读不到 seedDeviceID 写的那份 ⇒ 每次随机一个 deviceId
			 *     （且只有扫码成功才落盘）；
			 *   · 换票/兜底走 r.deviceID() 读的是 `qrlogin/.mi.token` ⇒ 另一个 id。
			 * passport 看到的"设备"前后不一致，免密 serviceLogin 直接回
			 * `code=70036 系统错误：请稍后再试`（用户截图那条）。
			 */
			TokenPath: ".mi.token",
			/*
			 * ★★ 2026-11-22（第三次定位，由错误文案 host=api2.mina.mi.com 反推出来）：
			 *
			 * 扫码服务域必须是 **xiaomiio**（MIoT），不能用 micoapi。
			 *
			 * 中间一度改成 micoapi 是因为手机 App 报 70036 —— 但那个 70036 的真因是
			 * **长轮询协程在 wasm 里不推进**（票无人等待），与 sid 无关。抄
			 * /deepseek-work/xiaoai-speaker-console 的 micoapi 恰恰是错的：那是**小爱
			 * 音箱**项目，它的凭据本来就该落到 `api2.mina.mi.com`（小爱的域）。
			 *
			 * 用 micoapi 扫码的实际后果（用户实测错误文案）：
			 *   `扫码票据换票（host=api2.mina.mi.com）：STS 地址不在可信范围`
			 *   —— 拿到的 location 指向小爱登录服务，而我们要的是 MIoT 的
			 *   serviceToken（api.io.mi.com RC4 探针 / 设备同步都用它）。
			 *   同时 micoapi 的 passToken 拿去换 xiaomiio 也不给 location
			 *   （`serviceLogin 响应缺少 location/nonce/ssecurity`）。
			 *
			 * 用 xiaomiio 扫码时，lp 成功返回的 location 直接就是
			 * `https://sts.api.io.mi.com/sts?...`（服务器实测过），
			 * 换票一步到位，不需要 passToken 兜底。
			 */
			Sid:             "xiaomiio",
			LongPollTimeout: 25 * time.Second,
			Logger:          nil,
			Exchange:        qrRT.exchange,
			// IO 全部走宿主托管会话（Transport/Store 不注入就会退回 net/http +
			// 文件——wasm 里没有 socket，这条路在沙箱里根本不通，测试里则会
			// 静默打到真实小米接口）。
			Transport: qrRT,
			Store:     qrRT,
		})
		return qrClient
	}
	qrRT.setContext(ctx)
	return qrClient
}

// qrOptionsTokenPath 暴露给测试：Client 的 TokenPath 必须是不带 qrlogin/ 前缀的键
// （runtime 的 Get/Put 会自己加前缀；两处叠加会让 bootstrap 与换票用两个 deviceId，
// passport 回 70036 —— 2026-11-22 实测根因）。
func qrOptionsTokenPath() string { return ".mi.token" }

// qrStartTool 发起扫码：返回二维码内容与状态（前端渲染二维码并轮询）。
func qrStartTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	c := qrClientFor(ctx)
	// 新一轮扫码必须用干净会话（见 resetSession 注释）；等待中的有效二维码直接复用
	if st := c.Status(); st.State != "waiting" {
		if err := qrRT.resetSession(ctx); err != nil {
			return json.Marshal(map[string]any{"ok": false, "error": err.Error(), "state": "error"})
		}
	}
	st, err := c.Start()
	if err != nil {
		return json.Marshal(map[string]any{"ok": false, "error": err.Error(),
			"state": st.State, "message": st.Message})
	}
	return json.Marshal(map[string]any{
		"ok":         true,
		"state":      st.State,
		"message":    st.Message,
		"login_url":  st.LoginURL,
		"expires_in": st.ExpiresIn,
	})
}

// qrStatusTool 轮询扫码状态（前端 1~2 秒一次）。
//
// ★ 2026-11-22（70036 真因）：这里必须**顺手做一次长轮询**。
//
// qrlogin.Client 原本在 Start 里 `go c.wait(...)` 起后台协程等扫码。wasm
// c-shared 的 Go 运行时在导出函数返回后没有调度器，协程不会推进 —— 那张票
// 从来没人等，手机点「确认登录」时小米认为没有有效等待方，回
// `70036 系统错误，请稍候再试`。实测证据：qr_status 每次只花 3ms。
//
// 前端本来就在 2 秒轮一次，于是让它成为驱动源：每次调用做一次**有界**轮询
// （2.2s，落在平台 3000ms 执行预算内；服务端 hold 到有结果或自己超时，超时
// 就当"还没扫"继续下一次）。拿到 code=0 后照常走换票 + 账号登记。
func qrStatusTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	c := qrClientFor(ctx)
	/*
	 * ★★ 2026-11-22（用户「手机上说登录成功，PC 上还没有任何提示」第二次）：
	 *
	 * 这里原来给 2200ms，理由是"落在 manifest execution_timeout_ms=30000 内"。
	 * 但**部署配置会覆盖 manifest**：这台机器的 homex.yaml 写的是
	 * `plugin.execution_timeout_ms: 3000`（3 秒）。2.2s 的长轮询 + 换票 + 序列化
	 * 正好撞线 ⇒ 平台在 3s 处把调用掐掉，插件侧表现为"瞬时返回、什么都没拿到"，
	 * 于是永远停在「等待扫码」。
	 *
	 * 改成 **25 秒**（2026-11-22 最终定稿，前提是部署 execution_timeout_ms 已调到 30000）：
	 *
	 * 实测证明短轮询**必然触发小米风控**：800ms 超时 + 2s 间隔连问，第 6 次
	 * （约 12 秒后）开始返回 `403 Forbidden`，之后全部拒绝；而 25000ms 单次
	 * 轮询连问 3 次全部正常、无 403。小米 lp 是按"单位时间新建连接次数"
	 * 判定的 —— 断得越勤，越快被封。真人扫码确认通常要 20~30 秒，所以短轮询
	 * 等于"等用户确认前就把通道封死"。
	 *
	 * 长轮询的正确用法就是**一个连接挂到底**：服务端 hold 到有结果或自己超时
	 * （约 30~120s），我们给 25s 留出余量，前端仍每 2 秒调一次 —— 命中时立刻
	 * 返回 code=0，没命中就等下一个 tick，连接数极少，不触发风控。
	 *
	 * 前提：部署 `plugin.execution_timeout_ms` 必须 ≥30000（这台机器原来写 3000，
	 * 插件根本没法做长轮询；已调整为 30000）。
	 */
	/*
	 * ★★★ 20 秒（2026-11-23 最终定稿；历史取值 2200ms → 800ms → 25s → 5s → 1.2s → 20s）
	 *
	 * ## 为什么必须"长"：结果只在连接活着时才送达
	 *
	 * 实测证据链：
	 *   · alpha.196 用 25s 长轮询 → 手机确认后**账号成功出现**（用户实测 146294077）；
	 *   · alpha.197 改成 1.2s → **手机提示登录成功，平台完全没反应**。
	 *   两次唯一的差别就是挂住时长。
	 *
	 * 说明小米 lp 的扫码结果是在**当前这条连接**上推送的：用户点「确认」的那一刻
	 * 如果我们的连接已经断开，结果就丢了，之后再问也拿不到。短轮询几乎永远
	 * 不在"确认瞬间"上，所以必然卡在「等待扫码」。
	 * （更早 800ms 那次也是同样现象，我当时误判成精度问题。）
	 *
	 * 1.2s 的实测结论（0 次 403）只证明了"不会触发风控"，没证明"能收到结果"——
	 * 我把前者当成了后者的充分条件，这是这次改坏的直接原因。
	 *
	 * ## 为什么不会再把沙箱顶穿
	 *
	 * 关键在于**避免调用排队**，而不是缩短轮询：
	 *   · 20s < 部署 execution_timeout_ms(30000)，单次调用本身不会超时；
	 *   · 前端改成"拿到响应后 300ms 再发下一次"（见 ui/app.js）—— 上一次还没
	 *     返回就绝不会发下一次，平台侧队列里最多只有 1 个在等；
	 *   · Client.Poll 的 pollInFlight 再做一层兜底：真出现并发时新调用立即返回。
	 *
	 * 之前崩溃是"25s 轮询 + 前端每 2 秒发一次"：2 秒一次必然在上一次未返回时
	 * 又发一个，队列越堆越长，每个都要等前面跑完，总耗时顶穿 30 秒。
	 */
	c.Poll(20 * time.Second)
	st := c.Status()
	out := map[string]any{
		"ok":         true,
		"state":      st.State,
		"message":    st.Message,
		"login_url":  st.LoginURL,
		"expires_in": st.ExpiresIn,
		"user_id":    st.UserID,
	}
	// 排障字段：手机说成功而这里还 waiting 时，凭它判断是 403/网络/服务端未就绪
	if st.PollStatus != 0 || st.PollNote != "" {
		out["poll_status"] = st.PollStatus
		out["poll_note"] = st.PollNote
	}
	return json.Marshal(out)
}

// qrCancelTool 取消等待。
func qrCancelTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	c := qrClientFor(ctx)
	c.Cancel()
	qrRT.closeSession() // 等待作废，会话一并收尾（下次 Start 会重开干净的）
	return json.Marshal(map[string]any{"ok": true, "cancelled": true})
}

// qrSVGTool 登录二维码的内联 SVG（前端 <img>/innerHTML 直接用）。
func qrSVGTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	c := qrClientFor(ctx)
	st := c.Status()
	loginURL := st.LoginURL
	if st.State != "waiting" || loginURL == "" {
		if err := qrRT.resetSession(ctx); err != nil {
			return nil, err
		}
		st2, err := c.Start()
		if err != nil {
			return nil, err
		}
		loginURL = st2.LoginURL
	}
	svg, err := qrlogin.QRSVG(loginURL)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"ok": true, "svg": svg})
}

// ---------- 小工具 ----------

// qrStr 把 JSON 解出来的值转成字符串（**数字不能走 fmt.Sprint**）。
//
// # 为什么必须特判 float64（2026-11-22 实测，扫码一直 70016 的最终根因）
//
// `encoding/json` 把 JSON number 解成 `float64`，而 `fmt.Sprint(float64)` 用
// 最短表示 —— **大整数会被写成科学计数法**：
//
//	fmt.Sprint(float64(146294077))  →  "1.46294077e+08"
//	fmt.Sprint(float64(319))        →  "319"            （小数字恰好正常）
//
// 小米长轮询返回的 `userId` 就是一个大整数（如 146294077）。原来的实现
// 把它拼进 cookie 变成 `userId=1.46294077e+08`，passport 认不出这个用户 ⇒
// 免密 serviceLogin 一律回 `code=70016 登录验证失败`（实测：带正确 userId
// 时 code=0 并下发 sts.api.io.mem 的 location；不带/带错时 70016）。
//
// 用 FormatFloat('f', -1, 64) 得到精确十进制整数，且不会引入多余小数位。
func qrStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		// -1 = 用最小必要位数；'f' = 定点（绝不出现 e+08 这种指数形式）
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(v)
	}
}

func qrInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		// UseNumber 之后 code 是 json.Number；漏这一支会让 `{"code":1}`
		// 被当成 0（成功），直接把"未扫码"误判成"扫码通过"。
		if n, e := x.Int64(); e == nil {
			return int(n)
		}
		return 0
	case string:
		var n int
		fmt.Sscanf(x, "%d", &n)
		return n
	}
	return 0
}

// qrDeviceID 16 位大写 hex（与 qrlogin.randomHex 同形态，passport 接受的设备标识）。
func qrDeviceID() string {
	const hexdigits = "0123456789ABCDEF"
	b := make([]byte, 16)
	_, _ = crand.Read(b)
	for i := range b {
		b[i] = hexdigits[int(b[i])%16]
	}
	return string(b)
}

// qrMuTryLoad 换票落盘后让 Client 重载内存态（deviceId/passToken）。
// 失败不影响结果——落盘已经完成，下次实例重建自然会读到。
func qrMuTryLoad() bool {
	qrMu.Lock()
	c := qrClient
	qrMu.Unlock()
	if c == nil {
		return false
	}
	c.Load()
	return true
}
