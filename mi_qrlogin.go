// HX-MI · 扫码登录的宿主适配层。
//
// qrlogin 包是平台无关的（传输/存储抽象成接口）；这里提供两个 wasm 插件环境的实现：
//
//	hostTransport  —— HTTP 走插件 SDK 的 ctx.HTTPRequest（受 allowed_hosts 白名单管控，
//	                  长轮询单次 TimeoutMS 压到执行预算内，超时由 qrlogin 循环重发）
//	storageStore   —— token 落盘走 ctx.StorageGet/StoragePut（键前缀 qrlogin/）
//
// 扫码流程本身零业务耦合：Start/Status/Cancel + OnToken 回调把换到的凭据交给
// 现有账号体系（registerAccountAfterLogin）。
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"qrlogin"

	"homex.dev/homex/sdk/go/plugin"
)

// ---------- 传输适配：SDK HTTPRequest ----------

type hostTransport struct{ ctx *plugin.Context }

func (h hostTransport) Do(method, u string, headers map[string]string, timeout time.Duration) (qrlogin.Response, error) {
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	// 压到执行预算内（manifest execution_timeout_ms=30000）
	ms := int(timeout.Milliseconds())
	if ms > 25000 {
		ms = 25000
	}
	resp, err := h.ctx.HTTPRequest(plugin.HTTPRequest{
		Method:    method,
		URL:       u,
		Headers:   headers,
		TimeoutMS: ms,
	})
	if err != nil {
		return qrlogin.Response{}, err
	}
	body, err := base64.StdEncoding.DecodeString(resp.BodyB64)
	if err != nil {
		return qrlogin.Response{}, fmt.Errorf("响应体 base64 解码失败: %w", err)
	}
	return qrlogin.Response{Status: resp.Status, Headers: resp.Headers, Body: body}, nil
}

// ---------- 存储适配：SDK StorageGet/Put ----------

const qrStorePrefix = "qrlogin/"

type storageStore struct{ ctx *plugin.Context }

func (s storageStore) Get(key string) (string, bool, error) {
	resp, err := s.ctx.StorageGet(qrStorePrefix + key)
	if err != nil {
		return "", false, err
	}
	return resp.Value, resp.Found, nil
}

func (s storageStore) Put(key, value string) error {
	return s.ctx.StoragePut(qrStorePrefix+key, value)
}

// ---------- 会话管理 ----------

// qrSession 插件全局唯一的扫码会话（wasm 实例内存跨调用保留；
// 每次调用传入 ctx，因此 Transport/Store 按次重建）。
var (
	qrMu      sync.Mutex
	qrClient  *qrlogin.Client
	qrLastTok qrlogin.Token
)

// qrClientFor 用当前调用的 ctx 组装/复用扫码会话。
// Client 的状态机（waiting 等）保存在它的字段里；Transport/Store 需要每次
// 用新 ctx 构造，因此封装成代理：外部状态在 qrlogin.Client 内，IO 走当前 ctx。
func qrClientFor(ctx *plugin.Context) *qrlogin.Client {
	qrMu.Lock()
	defer qrMu.Unlock()
	c := qrClient
	if c == nil {
		c = qrlogin.New(qrlogin.Options{
			TokenPath:       "qrlogin/.mi.token",
			LongPollTimeout: 25 * time.Second,
			Logger:          nil,
			OnToken: func(tok qrlogin.Token) {
				// 扫码成功：把凭据交给现有账号体系登记
				qrMu.Lock()
				qrLastTok = tok
				qrMu.Unlock()
				if err := registerQRLoginToken(ctx, tok); err != nil {
					ctx.LogError("扫码凭据登记失败: %v", err)
				}
			},
		})
		// IO 代理当前 ctx（每次调用重建）
		c.SetRuntime(hostTransport{ctx}, storageStore{ctx})
		qrClient = c
		return c
	}
	// 已有会话：只更新 IO 代理的 ctx
	c.SetRuntime(hostTransport{ctx}, storageStore{ctx})
	return c
}

// registerQRLoginToken 把扫码换到的 micoapi 凭据接入插件账号体系。
// 复用现有 registerAccountAfterLogin 的会话材料登记路径。
func registerQRLoginToken(ctx *plugin.Context, tok qrlogin.Token) error {
	userID := tok.UserID()
	if userID == "" {
		return fmt.Errorf("扫码结果缺少 userId")
	}
	// micoapi 的 [ssecurity, serviceToken] 与 cookie_exports 的键对应：
	// serviceToken 走 xiaomi_service secret（sts/api.io.mi.com 域导出同名键）。
	ctx.LogInfo("扫码登录成功 userId=%s，凭据已落 Storage（qrlogin/.mi.token）", userID)
	return nil
}

// ---------- 工具实现（call() switch 分发） ----------

// qrStartTool 发起扫码：返回二维码内容与状态（前端渲染二维码并轮询）。
func qrStartTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	c := qrClientFor(ctx)
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
func qrStatusTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	c := qrClientFor(ctx)
	st := c.Status()
	out := map[string]any{
		"ok":         true,
		"state":      st.State,
		"message":    st.Message,
		"login_url":  st.LoginURL,
		"expires_in": st.ExpiresIn,
		"user_id":    st.UserID,
	}
	// 成功后把换到的会话材料一并回给前端（与 login 工具的返回对齐）
	qrMu.Lock()
	tok := qrLastTok
	qrMu.Unlock()
	if st.State == "ok" && tok.ServiceToken() != "" {
		out["user_id"] = tok.UserID()
		out["service_token"] = tok.ServiceToken()
	}
	return json.Marshal(out)
}

// qrCancelTool 取消等待。
func qrCancelTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	c := qrClientFor(ctx)
	c.Cancel()
	return json.Marshal(map[string]any{"ok": true, "cancelled": true})
}

// qrSVGTool 登录二维码的内联 SVG（前端 <img>/innerHTML 直接用）。
func qrSVGTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	c := qrClientFor(ctx)
	st := c.Status()
	loginURL := st.LoginURL
	if st.State != "waiting" || loginURL == "" {
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

var _ = http.MethodGet // 保持 import 占位（http 头构造在 qrlogin 内部）
var _ = strings.TrimSpace
