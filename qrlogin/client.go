// Package qrlogin 提供小米账号「扫码登录」的自包含实现。
//
// 流程（对照官方 passport 协议）：
//
//  1. GET /pass/serviceLogin?sid=micoapi&_json=true      → location（登录页参数）
//  2. GET /longPolling/loginUrl?<params>                 → 二维码链接 + lp 轮询地址
//  3. GET <lp>（长轮询，手机确认后返回）                    → code=0 + nonce/ssecurity
//  4. GET <location>&clientSign=base64(sha1(nonce&ssecurity)) → 回调种下 serviceToken
//  5. 组装 token（miservice 格式）落盘 .mi.token + auth.json
//
// 本包不依赖任何业务代码：拷贝整个目录（含 go.mod）即可在其他项目使用。
// 可选能力：
//   - Refresh()：用已保存的 passToken 免密续期 serviceToken（24 小时有效，
//     建议每 12 小时调一次 + 30 分钟探活）
//   - Load()/HasToken()：重启后从磁盘恢复登录态
//   - QRSVG()：把登录链接渲染成内联 SVG（前端 <img>/innerHTML 直接用）
package qrlogin

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	sid         = "micoapi"
	defaultPass = "https://account.xiaomi.com/pass"
	longPolling = "https://account.xiaomi.com/longPolling/loginUrl"
)

// Status 扫码会话状态。
type Status struct {
	State     string `json:"state"`      // idle | waiting | ok | error
	Message   string `json:"message"`    // 人类可读的说明
	LoginURL  string `json:"login_url"`  // 二维码内容（waiting 时非空）
	ExpiresIn int    `json:"expires_in"` // 二维码剩余有效秒数
	UserID    string `json:"user_id"`    // 成功后非空
}

// Client 扫码登录会话管理。
type Client struct {
	mu       sync.Mutex
	inner    Token
	state    string
	message  string
	loginURL string
	deadline time.Time
	userID   string
	waitStop chan struct{}
	waiting  bool
	o        Options
	tr       Transport
	store    storeBucket
}

// Options 构造参数。零值可用（TokenPath 必填才有落盘）。
// 受限运行时（wasm 插件等）注入自己的 Transport 与 Store；
// 不注入时使用默认的 net/http + 文件。
type Options struct {
	TokenPath   string       // .mi.token 落盘键；Store 决定它落在哪里
	Passport    string       // passport 基址，默认官方；测试时指向 httptest
	HTTP        *http.Client // 长轮询用（默认 90s 超时）；Transport 为 nil 时生效
	ServiceHTTP *http.Client
	Logger      *log.Logger // nil = 标准日志
	OnToken     func(Token) // 扫码成功（已落盘）后的回调

	// 可插拔底层（默认 net/http + 文件）：
	Transport       Transport
	Store           Store
	LongPollTimeout time.Duration // 长轮询单次超时，默认 90s；wasm 建议 25s
}

// New 创建会话管理器并从磁盘加载已有登录态。
func New(o Options) *Client {
	if o.Passport == "" {
		o.Passport = defaultPass
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 90 * time.Second}
	}
	if o.Transport == nil {
		client := o.HTTP
		if client == nil {
			client = o.ServiceHTTP
		}
		o.Transport = &NetHTTPTransport{Client: client}
	}
	if o.LongPollTimeout <= 0 {
		o.LongPollTimeout = 90 * time.Second
	}
	c := &Client{state: "idle", o: o, tr: o.Transport}
	if o.Store != nil {
		c.store = storeBucket{store: o.Store}
	} else {
		c.store = storeBucket{store: FileStore{}}
	}
	c.inner = c.loadToken()
	return c
}

// lpTimeout 长轮询超时（受限运行时按执行预算收敛）。
func (c *Client) lpTimeout() time.Duration {
	if c.o.LongPollTimeout > 0 {
		return c.o.LongPollTimeout
	}
	return 90 * time.Second
}

// SetRuntime 运行时替换传输/存储代理。
// 用途：wasm 插件每次工具调用传入的宿主上下文不同（ctx 持有当前请求的
// host-call 通道），会话状态在 Client 内长期保留，IO 代理按次替换。
func (c *Client) SetRuntime(tr Transport, st Store) {
	c.mu.Lock()
	c.tr = tr
	if st != nil {
		c.store = storeBucket{store: st}
	}
	c.mu.Unlock()
}

func (c *Client) logf(format string, args ...any) {
	if c.o.Logger != nil {
		c.o.Logger.Printf("[qrlogin] "+format, args...)
		return
	}
	log.Printf("[qrlogin] "+format, args...)
}

// ---------- 对外 API ----------

// Start 发起扫码：返回 waiting 状态与登录链接。
// 已有未过期二维码时直接复用（避免用户反复刷新）。
func (c *Client) Start() (Status, error) {
	c.mu.Lock()
	if c.state == "waiting" && time.Now().Before(c.deadline) {
		st := c.statusLocked()
		c.mu.Unlock()
		return st, nil
	}
	c.cancelLocked()
	c.state = "waiting"
	c.message = "等待扫码"
	c.userID = ""
	c.mu.Unlock()

	data, err := c.bootstrap()
	if err != nil {
		c.mu.Lock()
		c.state = "error"
		c.message = fmt.Sprintf("获取二维码失败：%v", err)
		st := c.statusLocked()
		c.mu.Unlock()
		return st, fmt.Errorf("%v", err)
	}

	loginURL := strOf(data["loginUrl"])
	timeout := floatOf(data["timeout"], 240)
	c.mu.Lock()
	c.loginURL = loginURL
	c.deadline = time.Now().Add(time.Duration(timeout * float64(time.Second)))
	st := c.statusLocked()
	stop := make(chan struct{})
	c.waitStop = stop
	c.waiting = true
	c.mu.Unlock()

	go c.wait(data, stop)
	c.logf("已生成登录二维码，有效期 %.0fs", time.Until(c.deadline).Seconds())
	return st, nil
}

// Status 当前状态快照（前端 1~2 秒轮询一次）。
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusLocked()
}

// Cancel 取消等待中的扫码。
func (c *Client) Cancel() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelLocked()
	if c.state == "waiting" {
		c.state = "idle"
		c.message = ""
	}
}

func (c *Client) statusLocked() Status {
	in := 0
	if !c.deadline.IsZero() {
		in = int(time.Until(c.deadline).Seconds())
		if in < 0 {
			in = 0
		}
	}
	showURL := ""
	if c.state == "waiting" {
		showURL = c.loginURL
	}
	return Status{
		State: c.state, Message: c.message, LoginURL: showURL,
		ExpiresIn: in, UserID: c.userID,
	}
}

func (c *Client) cancelLocked() {
	if c.waitStop != nil {
		select {
		case <-c.waitStop:
		default:
			close(c.waitStop)
		}
		c.waitStop = nil
	}
	c.waiting = false
}

// ---------- 扫码流程 ----------

// bootstrap serviceLogin → location → longPolling/loginUrl → 二维码链接。
func (c *Client) bootstrap() (map[string]any, error) {
	headers := map[string]string{"User-Agent": randomUA()}
	// 不带这套 cookie 小米会回 HTML 页而不是 JSON（实测）
	deviceID := c.inner.str("deviceId")
	if deviceID == "" {
		deviceID = randomHex(16)
	}
	headers["Cookie"] = fmt.Sprintf("sdkVersion=3.9; deviceId=%s; passToken=", deviceID)

	body, _, err := c.httpGet(c.o.Passport+"/serviceLogin?sid="+sid+"&_json=true", headers)
	if err != nil {
		return nil, err
	}
	data := map[string]any{}
	if json.Unmarshal(passportStrip(body), &data) != nil {
		return nil, fmt.Errorf("serviceLogin 返回异常: %.120s", body)
	}
	loc := strOf(data["location"])
	if loc == "" {
		return nil, fmt.Errorf("%s", strOf(data["description"]))
	}
	u, err := url.Parse(loc)
	if err != nil {
		return nil, err
	}
	params := u.Query()
	params.Set("theme", "")
	params.Set("bizDeviceType", "")
	params.Set("_hasLogo", "false")
	params.Set("_qrsize", "240")
	params.Set("_dc", fmt.Sprint(time.Now().UnixMilli()))
	// longPolling/loginUrl 在 passport 根下，不在 /pass 里
	lpURL := strings.TrimSuffix(c.o.Passport, "/pass") + "/longPolling/loginUrl?" + params.Encode()
	body, _, err = c.httpGet(lpURL, headers)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if json.Unmarshal(passportStrip(body), &out) != nil {
		return nil, fmt.Errorf("longPolling/loginUrl 返回异常: %.120s", body)
	}
	return out, nil
}

// wait 长轮询等扫码；成功组装 token 落盘并回调。
func (c *Client) wait(data map[string]any, stop chan struct{}) {
	headers := map[string]string{"User-Agent": randomUA()}
	lpURL := strOf(data["lp"])
	if lpURL == "" {
		c.fail("登录响应缺少 lp 地址")
		return
	}
	// 长轮询循环：服务端 hold 到有结果或自身超时（返回非 0 code）。
	// 受限运行时（wasm）单次请求有执行预算上限（如 25s），超时就重发，
	// 直到二维码过期或用户取消——官方协议支持重复轮询。
	for {
		select {
		case <-stop:
			return // 用户取消，安静退出
		default:
		}
		c.mu.Lock()
		deadlineOK := time.Now().Before(c.deadline)
		c.mu.Unlock()
		if !deadlineOK {
			c.fail("二维码已过期，请重新获取")
			return
		}

		resp, lpErr := c.trDo(lpURL, headers, c.lpTimeout())
		if lpErr != nil {
			// 超时/网络错误：短暂退避后重发（不是失败——手机还没扫）
			time.Sleep(300 * time.Millisecond)
			continue
		}
		body := resp.Body

		out := map[string]any{}
		if json.Unmarshal(passportStrip(body), &out) != nil {
			c.fail("长轮询返回异常: " + trunc(string(body), 120))
			return
		}
		if code := intOf(out["code"]); code != 0 {
			// 服务端超时/未扫：继续下一轮轮询
			continue
		}
		c.logf("扫码通过（长轮询字段：%s）", keysOf(out))
		c.handleScanResult(out, headers, stop)
		return
	}
}

// handleScanResult 扫码成功后的换取与落盘（从 wait 中拆出，便于循环调用）。
func (c *Client) handleScanResult(out map[string]any, headers map[string]string, stop chan struct{}) {
	// 关键：location 必须带 clientSign（sha1(nonce&ssecurity) 的 base64）再取，
	// 否则小米只回一个页面、不种 serviceToken
	loc := strOf(out["location"])
	nonce, ssec := strOf(out["nonce"]), strOf(out["ssecurity"])
	serviceToken := ""
	if loc != "" && nonce != "" && ssec != "" {
		sign := clientSign(nonce, ssec)
		sep := "&"
		if !strings.Contains(loc, "?") {
			sep = "?"
		}
		jar, err := c.httpCookieJar(loc+sep+"clientSign="+url.QueryEscape(sign), headers, stop)
		if err == nil {
			serviceToken = jar["serviceToken"]
		}
		if serviceToken == "" {
			if b, _, e := c.httpGet(loc+sep+"clientSign="+url.QueryEscape(sign), headers); e == nil {
				b = []byte(strings.TrimSpace(string(b)))
				if len(b) > 0 && len(b) < 512 {
					serviceToken = string(b)
				}
			}
		}
		if serviceToken != "" {
			c.logf("已用 clientSign 换到 serviceToken")
		}
	}

	// 兜底：拿扫码出来的 passToken 走 serviceLogin 免密流程再换一次
	if serviceToken == "" {
		c.logf("签名回调没换到 serviceToken，改用 passToken 换取")
		tok := c.tokenSnapshot()
		tok["deviceId"] = randomHex(16)
		if v := strOf(out["userId"]); v != "" {
			tok["userId"] = v
		}
		if pt := strOf(out["passToken"]); pt != "" {
			tok["passToken"] = pt
		}
		c.setToken(tok)
		if state, why := c.miLogin(); state == "ok" {
			c.success(c.tokenSnapshot().str("userId"))
		} else {
			c.fail("换取 serviceToken 失败：" + why)
		}
		return
	}

	tok := Token{}
	if v := strOf(out["deviceId"]); v != "" {
		tok["deviceId"] = v
	} else {
		tok["deviceId"] = randomHex(16)
	}
	if v := strOf(out["userId"]); v != "" {
		tok["userId"] = v
	}
	if v := strOf(out["passToken"]); v != "" {
		tok["passToken"] = v
	}
	tok[sid] = []string{ssec, serviceToken}
	tok["saveTime"] = time.Now().UnixMilli()
	c.persist(tok)
	c.success(strOf(tok["userId"]))
}

func (c *Client) fail(msg string) {
	c.mu.Lock()
	if c.state == "waiting" {
		c.state = "error"
		c.message = msg
	}
	c.cancelLocked()
	c.mu.Unlock()
	c.logf("扫码登录结束：%s", msg)
}

func (c *Client) success(uid string) {
	c.mu.Lock()
	c.state = "ok"
	c.message = "登录成功"
	c.userID = uid
	c.cancelLocked()
	tokPath := c.o.TokenPath
	c.mu.Unlock()
	c.logf("扫码登录成功 userId=%s，token 落盘于 %s", uid, tokPath)
}

func keysOf(m map[string]any) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return strings.Join(ks, ",")
}

// ---------- 免密续期（serviceLogin）----------

// Refresh 用已保存的 passToken 免密换一个新的 serviceToken。
// serviceToken 24 小时过期；建议每 12 小时主动调一次，401/403 后补救调用。
// 返回 userID。
func (c *Client) Refresh() (string, error) {
	if c.tokenSnapshot().str("passToken") == "" {
		return "", fmt.Errorf("没有 passToken，需要扫码登录")
	}
	state, why := c.miLogin()
	if state != "ok" {
		return "", fmt.Errorf("续期失败：%s", why)
	}
	return c.tokenSnapshot().str("userId"), nil
}

// miLogin 免密流程（对照 miservice 语义，但把回调响应的 Set-Cookie 全收下：
// 小米会在这里轮换 passToken，丢了它下次续期必 70016）。
func (c *Client) miLogin() (string, string) {
	tok := c.tokenSnapshot()
	deviceID := tok.str("deviceId")
	if deviceID == "" {
		deviceID = randomHex(16)
		tok["deviceId"] = deviceID
	}
	delete(tok, "cUserId") // 清掉上一轮残留
	c.setToken(tok)

	cookie := fmt.Sprintf("sdkVersion=3.9; deviceId=%s; passToken=%s",
		deviceID, tok.str("passToken"))
	if uid := tok.str("userId"); uid != "" {
		cookie = fmt.Sprintf("sdkVersion=3.9; deviceId=%s; userId=%s; passToken=%s",
			deviceID, uid, tok.str("passToken"))
	}
	body, status, err := c.httpGetSvc(c.o.Passport+"/serviceLogin?sid="+sid+"&_json=true",
		map[string]string{"Cookie": cookie})
	if err != nil {
		return "fail", fmt.Sprintf("serviceLogin: %v", err)
	}
	resp, err := parseMiJSON(body, status)
	if err != nil {
		return "fail", err.Error()
	}
	if intOf(resp["code"]) != 0 {
		return "need_password", fmt.Sprintf("code=%d %s",
			intOf(resp["code"]), strOf(resp["description"]))
	}
	if strOf(resp["location"]) == "" || strOf(resp["nonce"]) == "" || strOf(resp["ssecurity"]) == "" {
		return "fail", "响应缺少 location/nonce/ssecurity"
	}

	tok = c.tokenSnapshot()
	if v := strOf(resp["passToken"]); v != "" {
		tok["passToken"] = v // 仅在服务端轮换时覆盖
	}
	c.setToken(tok)

	// clientSign 回调：全收 Set-Cookie（轮换后的 passToken/cUserId 在这里）
	sign := clientSign(strOf(resp["nonce"]), strOf(resp["ssecurity"]))
	loc := strOf(resp["location"])
	sep := "&"
	if !strings.Contains(loc, "?") {
		sep = "?"
	}
	jar, err := c.httpCookieJar(
		loc+sep+"clientSign="+url.QueryEscape(sign), nil, nil)
	if err != nil {
		log.Printf("[qrlogin] 取 serviceToken 回调异常：%v", err)
	}
	serviceToken := jar["serviceToken"]
	if serviceToken == "" {
		if b, _, e := c.httpGet(loc+sep+"clientSign="+url.QueryEscape(sign), nil); e == nil {
			b = []byte(strings.TrimSpace(string(b)))
			if len(b) > 0 && len(b) < 512 {
				serviceToken = string(b)
			}
		}
	}
	if serviceToken == "" {
		return "fail", "回调里没有 serviceToken"
	}

	tok = c.tokenSnapshot()
	rotated := false
	for _, k := range []string{"passToken", "cUserId", "userId", "yetAnotherServiceToken"} {
		v := jar[k]
		if v == "" {
			continue
		}
		if k == "userId" {
			tok["userId"] = v
		} else if tok.str(k) != v {
			if k == "passToken" {
				rotated = true
			}
			tok[k] = v
		}
	}
	if rotated {
		c.logf("小米轮换了 passToken，已采用新值")
	}
	tok[sid] = []string{strOf(resp["ssecurity"]), serviceToken}
	tok["saveTime"] = time.Now().UnixMilli()
	c.persist(tok)
	return "ok", ""
}

// clientSign passport 的规矩：location 必须附 clientSign（sha1(nonce&ssecurity) 的
// base64），否则只返回一个页面、不种 cookie。
func clientSign(nonce, ssecurity string) string {
	nsec := "nonce=" + nonce + "&" + ssecurity
	sum := sha1Sum([]byte(nsec))
	return base64Std(sum)
}
