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
	 * 换票按**服务域**逐个试（2026-11-22 用户手机截图 70036 之后的定案）：
	 *
	 *   ① passToken → xiaomiio：MIoT 云（api.io.mi.com RC4 探针 / 设备同步）要的就是
	 *      这个域的 serviceToken，所以它是**主路径**；
	 *   ② passToken → micoapi：与 console 完全一致的域，主路径不通时的等价退路；
	 *   ③ lp 直接给的 (location, ssecurity)：扫码会话自己的票据，最后再试一次。
	 *
	 * 每次都用**新生成的 deviceId**（console 的做法：tok["deviceId"]=randomHex(16)）。
	 * 沿用扫码那台的 id 会让 passport 把"新设备 + 老 passToken"判成异常。
	 */
	var errs []string
	bindAccount := func(uid, pt, dev string) {
		r.finish(uid, pt, dev)
	}

	s.State = "failed"
	for _, sid := range []string{"xiaomiio", "micoapi"} {
		if passToken == "" {
			errs = append(errs, "passToken 缺失，跳过 "+sid)
			continue
		}
		freshID := qrDeviceID()
		loc2, ssec2, pt2, ferr := r.serviceLoginWithPassToken(ctx, sess, sid, freshID, userID, passToken)
		if ferr != nil {
			errs = append(errs, fmt.Sprintf("%s 取票：%v", sid, ferr))
			continue
		}
		if _, e2 := exchangeAndProbe(ctx, sess, s, loc2, ssec2, pt2); e2 == nil {
			bindAccount(userID, pt2, freshID)
			return userID, nil
		} else {
			errs = append(errs, fmt.Sprintf("%s 换票：%v", sid, e2))
		}
	}

	// ③ 扫码会话自己给的票据（clientSign 回调）
	if loc != "" && nonce != "" && ssec != "" {
		s.State = "failed"
		if _, e3 := exchangeAndProbe(ctx, sess, s, withClientSign(loc, nonce, ssec), ssec, passToken); e3 == nil {
			bindAccount(userID, passToken, deviceID)
			return userID, nil
		} else {
			errs = append(errs, "扫码票据换票："+e3.Error())
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
func (r *qrRuntime) serviceLoginWithPassToken(ctx *plugin.Context, sess, sid, deviceID, userID, passToken string) (string, string, string, error) {
	if sid == "" {
		sid = "xiaomiio"
	}
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
		return "", "", "", fmt.Errorf("serviceLogin: %w", err)
	}
	body, err := resp.Body()
	if err != nil {
		return "", "", "", err
	}
	raw := strings.TrimSpace(strings.ReplaceAll(string(body), "&&&START&&&", ""))
	var d map[string]any
	if json.Unmarshal([]byte(raw), &d) != nil {
		return "", "", "", fmt.Errorf("serviceLogin 返回异常: %.120s", raw)
	}
	if code := qrInt(d["code"]); code != 0 {
		return "", "", "", fmt.Errorf("serviceLogin code=%d %s", code, qrStr(d["description"]))
	}
	loc := qrStr(d["location"])
	nonce := qrStr(d["nonce"])
	ssec := qrStr(d["ssecurity"])
	if loc == "" || nonce == "" || ssec == "" {
		return "", "", "", fmt.Errorf("serviceLogin 响应缺少 location/nonce/ssecurity")
	}
	if pt := qrStr(d["passToken"]); pt != "" {
		passToken = pt // 服务端轮换时采用新值
	}
	return withClientSign(loc, nonce, ssec), ssec, passToken, nil
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
			 * ★★ 2026-11-22（用户手机截图：小米 App 里直接显示
			 * 「系统错误，请稍候再试:70036」）：
			 *
			 * 这条错误是**小米 App 侧**弹的，不是我们的代码抛的 —— 说明"确认登录"
			 * 这一步就被对方拒了，问题在二维码/服务域，而不在换票。对照
			 * /deepseek-work/xiaoai-speaker-console（同机、同 App、实测正常）：
			 * 它扫码用的是 **sid=micoapi**。
			 *
			 * 结论：**扫码阶段用 micoapi**；xiaomiio（MIoT 云要用的域）在拿到
			 * passToken 之后再用免密 serviceLogin 单独换（见 exchange）。
			 */
			Sid:             "micoapi",
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
	c.Poll(2200 * time.Millisecond)
	st := c.Status()
	out := map[string]any{
		"ok":         true,
		"state":      st.State,
		"message":    st.Message,
		"login_url":  st.LoginURL,
		"expires_in": st.ExpiresIn,
		"user_id":    st.UserID,
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

func qrStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func qrInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
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
