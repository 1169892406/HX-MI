# qrlogin — 小米账号扫码登录（自包含 Go 包）

从小爱音响控制台项目抽出的独立模块：**拷贝整个目录（含 go.mod）即可移植到任何 Go 项目**。

- 二维码登录全流程：serviceLogin → longPolling/loginUrl → 长轮询 → clientSign 回调 → token 组装
- token 落盘：`.mi.token`（miservice 兼容格式）+ `auth.json`（根凭据，只增不删）
- 免密续期 `Refresh()`：passToken 换新 serviceToken（**正确处理 passToken 轮换**——
  miservice 把回调 Set-Cookie 里的新 passToken 丢了，导致 70016 死循环只能重新扫码，
  这是同类项目最常见的坑，本包已修）
- 二维码 SVG 渲染（`rsc.io/qr`，前端零依赖）
- 官方协议细节对齐：X-Timestamp 格式与 `Z` 后缀（微软 bug 兼容）、`&&&START&&&`
  前缀剥离、随机机型 UA、长轮询地址在 passport 根下

## 快速上手

```go
c := qrlogin.New(qrlogin.Options{
    TokenPath: "conf/.mi.token",          // 同目录会写 auth.json
    OnToken: func(tok qrlogin.Token) {    // 扫码成功（已落盘）
        fmt.Println("登录成功 userId =", tok.UserID())
    },
})

// 1) 发起扫码，拿二维码内容
st, _ := c.Start()
// st.LoginURL → 渲染二维码；前端轮询 c.Status()

// 2) 轮询状态（state: idle|waiting|ok|error）
st = c.Status()
if st.State == "ok" { /* st.UserID 即登录用户 */ }

// 3) 免密续期（serviceToken 24h 过期；建议 12h 一次 + 401 后补救）
uid, err := c.Refresh()

// 4) 二维码 SVG（HTTP 直接输出）
mux.Handle("/qr/svg", c.QRSVGHandler())
```

## Token 格式（miservice 兼容）

```json
{
  "deviceId": "…",
  "userId": "319",
  "passToken": "…",
  "micoapi": ["<ssecurity>", "<serviceToken>"],
  "cUserId": "…",
  "saveTime": 1795229986
}
```

`micoapi` 键与 `miservice`（Python）约定一致：`[ssecurity, serviceToken]`。
已有的 xiaomusic / miservice token 文件可以直接复用（扁平格式会自动规范化）。

## 测试

```bash
cd qrlogin && go test ./...
```

不联网：httptest 模拟 passport（bootstrap → 长轮询 → clientSign 回调），
覆盖 token 落盘、passToken 轮换被收下、70016 不破坏现有凭据三组语义。

## 环境要求

Go 1.24+；唯一外部依赖 `rsc.io/qr`（二维码编码，纯 Go）。

## 协议要点（踩坑记录）

1. `serviceLogin` 必须带 `Cookie: sdkVersion=3.9; deviceId=…; passToken=`，
   否则小米返回 HTML 而不是 JSON。
2. `longPolling/loginUrl` 在 `account.xiaomi.com` **根下**，不在 `/pass` 里。
3. 扫码回调的 location 必须附 `clientSign`（`base64(sha1("nonce="+nonce+"&"+ssecurity))`），
   裸 GET 只会拿到一个页面。
4. 回调响应的 **Set-Cookie 全部要收**：`serviceToken` 之外还有轮换的
   `passToken`/`cUserId`——丢了就 70016。
5. 续期失败（70016）时**绝不能**用 `.bak` 里的旧凭据覆盖刚轮换的新值。
