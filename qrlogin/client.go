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
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// sid 服务域参数与 token 键名。Sid() 取运行时配置（默认 micoapi）。
const (
	sid         = "micoapi"
	defaultPass = "https://account.xiaomi.com/pass"
	longPolling = "https://account.xiaomi.com/longPolling/loginUrl"
)

func (c *Client) sid() string {
	if c.o.Sid != "" {
		return c.o.Sid
	}
	return sid
}

// Status 扫码会话状态。
type Status struct {
	State    string `json:"state"`     // idle | waiting | ok | error
	Message  string `json:"message"`   // 人类可读的说明
	LoginURL string `json:"login_url"` // 二维码内容（waiting 时非空）
	// PollStatus / PollNote 是最近一次长轮询的诊断（HTTP 状态码 + 错误或响应首段）。
	// 只用于排障：手机说"登录成功"而插件停在等待时，凭它就能判断是
	// "请求被掐(403)" / "网络没通" / "服务端还没结果"，不必靠猜。
	PollStatus int    `json:"poll_status,omitempty"`
	PollNote   string `json:"poll_note,omitempty"`
	ExpiresIn  int    `json:"expires_in"` // 二维码剩余有效秒数
	UserID     string `json:"user_id"`    // 成功后非空
}

// Client 扫码登录会话管理。
type Client struct {
	mu       sync.Mutex
	inner    Token
	state    string
	message  string
	loginURL string
	/* 长轮询地址与请求头（Poll 用；wait 也用同一份） */
	lpURL     string
	lpHeaders map[string]string
	/* 最近一次轮询诊断 */
	pollStatus int
	pollNote   string
	/*
	 * pollInFlight 防止 Poll 重入（2026-11-22 实测事故）。
	 *
	 * wasm 是**单线程**：一次 Poll 在等网络时整条执行流被占住，期间到达的
	 * qr_status（前端每 2 秒一次）与用户触发的 accounts 全部排队。Poll 原来给
	 * 25 秒，队列稍长就顶穿平台 30 秒上限 ⇒ `PLUGIN_SANDBOX_CRASH:
	 * 沙箱实例已不可用（module closed with context deadline exceeded）`，
	 * 用户看到的是「读取账号失败」。
	 *
	 * 有了这个标志：已有一次在飞时，新的 Poll **立刻返回**当前状态（前端下一拍
	 * 再来问），队列不可能堆积。
	 */
	pollInFlight bool
	deadline     time.Time
	userID       string
	waitStop     chan struct{}
	waiting      bool
	o            Options
	tr           Transport
	store        storeBucket
}

// Options 构造参数。零值可用（TokenPath 必填才有落盘）。
// 受限运行时（wasm 插件等）注入自己的 Transport 与 Store；
// 不注入时使用默认的 net/http + 文件。
type Options struct {
	TokenPath string // .mi.token 落盘键；Store 决定它落在哪里
	Passport  string // passport 基址，默认官方；测试时指向 httptest
	// Sid 登录服务域：默认 micoapi（小爱）；米家设备类接入用 xiaomiio。
	// 注意：内置组装路径（未设 Exchange）落盘的 token 文件键名固定为 micoapi
	// （miservice 兼容格式）——改 Sid 时应配合 Exchange，由宿主适配层自行组装凭据。
	Sid         string
	HTTP        *http.Client // 长轮询用（默认 90s 超时）；Transport 为 nil 时生效
	ServiceHTTP *http.Client
	Logger      *log.Logger // nil = 标准日志
	OnToken     func(Token) // 扫码成功（已落盘）后的回调

	// Exchange 接管"扫码确认后的换票"整段：长轮询拿到 code=0 的结果后，
	// Client 不再做 clientSign 回调 / Set-Cookie 组装 / 落盘，而是把原始
	// 结果（location/nonce/ssecurity/passToken/userId/deviceId…）交给它。
	//
	// 为什么需要：宿主托管 cookie 的运行时（wasm 插件的 HTTPSession）里，
	// Set-Cookie 永远不回插件、cookie 由宿主 jar 收发——插件侧根本"看不见"
	// serviceToken，内置的组装路径必失败。Exchange 返回 userID 视为成功；
	// 返回 error 则进入 error 状态。nil = 走内置路径（net/http 场景）。
	Exchange func(out map[string]any) (string, error)

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

	/*
	 * ★★ 2026-11-22（扫码一直 70036 的真因）：
	 *
	 * `go c.wait(...)` 在 **wasm 里跑不起来**。wasm c-shared 的 Go 运行时在
	 * 导出函数返回后就没有调度器了 —— 协程不会推进，宿主调用之间没有任何
	 * CPU 时间片。实测证据：前端每 2 秒调一次 qr_status，每次只花 **3ms**
	 * （如果长轮询真的在跑，至少会占住几十毫秒到 25 秒）。
	 *
	 * 于是那张票**从来没人轮询**：手机上点「确认登录」时，小米服务端认为
	 * 这个 ticket 没有有效等待方（lp 连接不存在），直接回
	 * `70036 系统错误，请稍候再试`。console 是原生进程、调度器常驻，所以它正常。
	 *
	 * 解法不是"修协程"，而是换驱动源：把长轮询改成**每次 qr_status 调用做一次
	 * 有界轮询**（见 Poll），由前端既有的 2 秒轮询驱动。这样完全不需要
	 * 后台协程，也不依赖执行预算之外的任何东西。
	 *
	 * 原生进程（console）仍可走协程：Poll 是幂等补充，二者不会重复处理
	 * （pollOnce 有状态锁，拿到结果就转 ok/error，等待循环随即退出）。
	 */
	c.mu.Lock()
	c.lpURL = strOf(data["lp"])
	c.lpHeaders = map[string]string{"User-Agent": randomUA()}
	c.mu.Unlock()
	/*
	 * ★★ 2026-11-22：**不再起后台协程**。
	 *
	 * 两重理由，都是实测出来的：
	 *  ① wasm c-shared 的 Go 运行时在导出函数返回后没有调度器，协程根本不推进
	 *     （证据：qr_status 曾长期只花 3ms，那张票从来没人等）；
	 *  ② 即使偶尔跑起来，它遇到一次 403/非 JSON 就会把 state 写成 error ——
	 *     而真正在驱动轮询的是 Poll（每次 qr_status 调一次）。让两个驱动源
	 *     互相覆盖状态，是"手机说成功、插件没反应"的成因之一。
	 *
	 * 现在等待只由 Poll 驱动（见 qr_status），状态只有一个写入者。
	 */
	_ = stop
	c.logf("已生成登录二维码，有效期 %.0fs", time.Until(c.deadline).Seconds())
	return st, nil
}

// Status 当前状态快照（前端 1~2 秒轮询一次）。
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusLocked()
}

// Poll 由**外部调用**驱动一次有界长轮询（2026-11-22，wasm 沙箱专用路径）。
//
// # 为什么需要它
//
// wasm c-shared 的 Go 运行时在导出函数返回后就没有调度器：`go c.wait(...)`
// 起的长轮询协程**不会推进**。结果那张 ticket 从来没人等，手机点确认登录时
// 小米回 70036（"系统错误，请稍后再试"，其实是没有有效等待方）。
// 实测证据：前端每 2 秒调一次 qr_status，每次只花 3ms。
//
// 既然"后台推进"在沙箱里不成立，就把驱动权交给宿主：前端本来就在轮询
// qr_status，那就让 qr_status 顺手做一次**有界**长轮询（timeout 由调用方给，
// 必须落在插件执行预算内）。
//
// 幂等性：拿到 code=0 结果就转 ok/error 并清掉 waiting，后续 Poll 直接返回当前
// 状态；原生进程里 wait 协程与 Poll 并存也安全（谁先拿到结果谁收尾，另一个
// 看到 state != waiting 就退出）。
func (c *Client) Poll(timeout time.Duration) {
	c.mu.Lock()
	if c.pollInFlight {
		// 已有一次长轮询在等网络：立刻返回，绝不排队（见 pollInFlight 注释）。
		c.mu.Unlock()
		return
	}
	/*
	 * 只要求"票还在有效期内且有 lp 地址"，**不要求 state==waiting**。
	 *
	 * 为什么要放宽（2026-11-22）：wait 协程（原生进程里会跑）曾把一次 403 判成
	 * error，此后 Poll 因为 state!=waiting 直接返回 ⇒ 用户手机明明确认成功，
	 * 插件这边再也拿不到结果。票没过期就继续问，是这里唯一正确的判据 ——
	 * 真过期由下面的 deadline 分支负责终止。
	 */
	if c.lpURL == "" || (c.state != "waiting" && c.state != "error") {
		c.pollStatus = -1
		c.pollNote = fmt.Sprintf("Poll 跳过：lpURL=%d state=%s", len(c.lpURL), c.state)
		c.mu.Unlock()
		return
	}
	if c.state == "error" && c.userID != "" {
		c.mu.Unlock()
		return
	}
	lpURL := c.lpURL
	headers := c.lpHeaders
	deadline := c.deadline
	c.pollInFlight = true
	c.mu.Unlock()
	// 无论走哪条分支都要清标志，否则轮询永久停摆。
	defer func() {
		c.mu.Lock()
		c.pollInFlight = false
		c.mu.Unlock()
	}()

	if !time.Now().Before(deadline) {
		c.mu.Lock()
		c.pollStatus = -2
		c.pollNote = "二维码已过期"
		c.mu.Unlock()
		c.fail("二维码已过期，请重新获取")
		return
	}
	if headers == nil {
		headers = map[string]string{"User-Agent": randomUA()}
	}
	startedAt := time.Now()
	resp, err := c.trDo(lpURL, headers, timeout)
	if err != nil {
		// 超时/网络错误不算失败：手机还没扫，下一次调用继续。
		// 但诊断要留下：否则"手机成功、插件无反应"只能靠猜。
		c.mu.Lock()
		c.pollStatus = 0
		c.pollNote = fmt.Sprintf("request 失败(%dms): %v", time.Since(startedAt).Milliseconds(), err)
		c.mu.Unlock()
		return
	}
	out := map[string]any{}
	if parsePassportJSON(resp.Body, &out) != nil {
		c.mu.Lock()
		c.pollStatus = resp.Status
		c.pollNote = fmt.Sprintf("非 JSON(%dms, %d bytes): %s", time.Since(startedAt).Milliseconds(),
			len(resp.Body), trunc(string(resp.Body), 90))
		c.mu.Unlock()
		return
	}
	if code := intOf(out["code"]); code != 0 {
		// 服务端"还没扫"也走这里（非 0 code），继续等
		c.mu.Lock()
		c.pollStatus = resp.Status
		c.pollNote = fmt.Sprintf("code=%d %s (%dms)", code, strOf(out["description"]),
			time.Since(startedAt).Milliseconds())
		c.mu.Unlock()
		return
	}
	c.logf("扫码通过（Poll 驱动，字段：%s）", keysOf(out))
	c.handleScanResult(out, headers, nil)
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
		PollStatus: c.pollStatus, PollNote: c.pollNote,
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

	body, _, err := c.httpGet(c.o.Passport+"/serviceLogin?sid="+c.sid()+"&_json=true", headers)
	if err != nil {
		return nil, err
	}
	data := map[string]any{}
	if parsePassportJSON(body, &data) != nil {
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
	if parsePassportJSON(body, &out) != nil {
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
		if parsePassportJSON(body, &out) != nil {
			/*
			 * ★ 2026-11-22（用户「app 提示扫码成功登录了，插件侧没有反应」）：
			 *
			 * 这里原来直接 `c.fail(...)`。而长轮询的非 JSON 响应（403/Hold 被中断的
			 * 风控页/网关错误页）**不代表扫码失败** —— 它只说明这一次请求被掐了。
			 * 一旦判死，用户手机上明明点了确认，插件却永远停在 error：
			 *   实测文案「长轮询返回异常: <!DOCTYPE HTML ... 403 Forbidden>」
			 *   而同一时刻 Poll（每次 qr_status 驱动的那种）还能拿到 code=0。
			 *
			 * 正确语义：**这次没拿到结果就继续下一次**（与 lpErr 超时同一处理）。
			 * 只有"二维码过期"和"用户取消"才是终止条件。
			 */
			c.logf("长轮询本次响应不可解析（按未扫继续）：%s", trunc(string(body), 80))
			time.Sleep(300 * time.Millisecond)
			continue
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
	// 宿主托管 cookie 的运行时（wasm HTTPSession）：换票整段由注入方完成。
	// 那里的 Set-Cookie 永远不回插件，内置的组装路径在那类环境必失败。
	if c.o.Exchange != nil {
		uid, err := c.o.Exchange(out)
		if err != nil {
			c.fail("换取会话失败：" + err.Error())
			return
		}
		if uid == "" {
			uid = strOf(out["userId"])
		}
		c.success(uid)
		return
	}

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
				if tokenLikeBody(b) {
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
			if tokenLikeBody(b) {
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
