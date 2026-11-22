package main

// 扫码登录宿主适配层的集成测试（hosttest 模拟宿主 ops）。
//
// 钉住的回归（2026-11-22，扫码 70036 排障）：
//  1. qr 出站必须走 HTTPSession（OpHTTPSessionRequest），不能走裸 OpHTTPRequest
//     —— 裸通道剥 Cookie 头 / 剥 Set-Cookie / 不跟随重定向；
//  2. 显式 Cookie 头（sdkVersion; deviceId; passToken=）必须原样透传给宿主；
//  3. 每次新扫码引导开一个干净会话（HTTPOpen），等待取消时收尾（HTTPClose）。

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

func qrSessionBody(v any) map[string]any {
	b, _ := json.Marshal(v)
	return map[string]any{
		"status":    200,
		"body_b64":  base64.StdEncoding.EncodeToString([]byte("&&&START&&&" + string(b))),
		"headers":   map[string]string{"Content-Type": "application/json"},
		"redirects": 0,
	}
}

func TestQRStartUsesHostSession(t *testing.T) {
	h := hosttest.New(t)
	// 设备标识落盘（首次生成）
	h.Fixture(plugin.OpStorageGet, map[string]any{"found": false})
	h.Fixture(plugin.OpStoragePut, map[string]any{"ok": true})
	openCalls := 0
	h.On(plugin.OpHTTPOpen, func(json.RawMessage) (any, error) {
		openCalls++
		return plugin.HTTPOpenResponse{SessionID: "sess-qr-1"}, nil
	})
	closeCalls := 0
	h.On(plugin.OpHTTPClose, func(json.RawMessage) (any, error) {
		closeCalls++
		return map[string]any{"closed": true}, nil
	})

	sessReqs := 0
	var loginCookie string
	h.On(plugin.OpHTTPSessionRequest, func(raw json.RawMessage) (any, error) {
		sessReqs++
		var req struct {
			SessionID string            `json:"session_id"`
			URL       string            `json:"url"`
			Headers   map[string]string `json:"headers"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if req.SessionID != "sess-qr-1" {
			t.Errorf("session_id = %q（期望 sess-qr-1）", req.SessionID)
		}
		switch {
		case strings.Contains(req.URL, "/serviceLogin"):
			loginCookie = req.Headers["Cookie"]
			if !strings.Contains(loginCookie, "sdkVersion=3.9") ||
				!strings.Contains(loginCookie, "deviceId=") ||
				!strings.Contains(loginCookie, "passToken=") {
				t.Errorf("serviceLogin 的 Cookie 头不完整: %q", loginCookie)
			}
			return qrSessionBody(map[string]any{
				"location": "https://account.xiaomi.com/fe/service/login?sid=xiaomiio&qs=%3Fsid%3Dxiaomiio",
				"callback": "https://sts.api.io.mi.com/sts", "_sign": "SIGN",
			}), nil
		case strings.Contains(req.URL, "/longPolling/loginUrl"):
			return qrSessionBody(map[string]any{
				"loginUrl": "https://account.xiaomi.com/qr/login?ticket=lp_TEST",
				"lp":       "https://lp.account.xiaomi.com/lp/s?k=lp_TEST",
				"timeout":  240,
			}), nil
		default: // 长轮询：一直"未扫"
			return qrSessionBody(map[string]any{"code": 1, "description": "waiting"}), nil
		}
	})

	// 裸 HTTPRequest 通道一次都不该被碰（回归：旧实现走它导致 70036）
	h.On(plugin.OpHTTPRequest, func(json.RawMessage) (any, error) {
		t.Error("扫码流程不应使用裸 http.request（Cookie/Set-Cookie/重定向都会断）")
		return nil, nil
	})

	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	out, err := qrStartTool(ctx, json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("qrStartTool: %v", err)
	}
	var res map[string]any
	if json.Unmarshal(out, &res) != nil {
		t.Fatalf("响应不是 JSON: %s", out)
	}
	if res["ok"] != true || res["state"] != "waiting" {
		t.Fatalf("start 响应异常: %v", res)
	}
	if res["login_url"] == "" {
		t.Fatal("未返回二维码链接")
	}
	if openCalls != 1 {
		t.Errorf("HTTPOpen 次数 = %d（期望 1：一次引导一个干净会话）", openCalls)
	}
	if sessReqs < 2 {
		t.Errorf("HTTPSessionRequest 次数 = %d（至少 serviceLogin + loginUrl）", sessReqs)
	}
	if !strings.Contains(loginCookie, "deviceId=") {
		t.Fatal("未记录到带 deviceId 的 Cookie（seedDeviceID 未生效？）")
	}
	// 设备标识已落盘，重启后仍是同一台"设备"
	var seeded map[string]any
	putSeen := false
	for _, call := range h.CallsOf(plugin.OpStoragePut) {
		var w struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		_ = json.Unmarshal(call.Request, &w)
		if strings.HasSuffix(w.Key, "qrlogin/.mi.token") {
			putSeen = true
			_ = json.Unmarshal([]byte(w.Value), &seeded)
		}
	}
	if !putSeen || qrStr(seeded["deviceId"]) == "" {
		t.Error("deviceId 未落盘（qrlogin/.mi.token 缺少 deviceId）")
	}

	// 收尾：取消等待 → 会话关闭
	if _, err := qrCancelTool(ctx, json.RawMessage("{}")); err != nil {
		t.Fatalf("qrCancelTool: %v", err)
	}
	if closeCalls == 0 {
		t.Error("取消后未关闭宿主会话（会累积挤占 4 会话上限）")
	}
}

func TestQRStatusReflectsState(t *testing.T) {
	h := hosttest.New(t)
	h.Fixture(plugin.OpStorageGet, map[string]any{"found": false})
	h.Fixture(plugin.OpStoragePut, map[string]any{"ok": true})
	h.Fixture(plugin.OpHTTPOpen, plugin.HTTPOpenResponse{SessionID: "sess-qr-2"})
	h.On(plugin.OpHTTPClose, func(json.RawMessage) (any, error) {
		return map[string]any{"closed": true}, nil
	})
	h.On(plugin.OpHTTPSessionRequest, func(raw json.RawMessage) (any, error) {
		var req struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(raw, &req)
		switch {
		case strings.Contains(req.URL, "/serviceLogin"):
			return qrSessionBody(map[string]any{
				"location": "https://account.xiaomi.com/fe/service/login?sid=xiaomiio",
				"callback": "https://sts.api.io.mi.com/sts", "_sign": "SIGN",
			}), nil
		case strings.Contains(req.URL, "/longPolling/loginUrl"):
			return qrSessionBody(map[string]any{
				"loginUrl": "https://account.xiaomi.com/qr/login?ticket=lp_T2",
				"lp":       "https://lp.account.xiaomi.com/lp/s?k=lp_T2",
				"timeout":  240,
			}), nil
		default:
			return qrSessionBody(map[string]any{"code": 1}), nil
		}
	})

	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	if _, err := qrStartTool(ctx, json.RawMessage("{}")); err != nil {
		t.Fatalf("qrStartTool: %v", err)
	}
	out, err := qrStatusTool(ctx, json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("qrStatusTool: %v", err)
	}
	var res map[string]any
	if json.Unmarshal(out, &res) != nil {
		t.Fatalf("响应不是 JSON: %s", out)
	}
	if res["state"] != "waiting" {
		t.Errorf("qr_status state = %v（期望 waiting）", res["state"])
	}
	if _, err := qrCancelTool(ctx, json.RawMessage("{}")); err != nil {
		t.Fatalf("qrCancelTool: %v", err)
	}
}

// 钉住 70036 的根因：Client 的 TokenPath 与 qrRuntime 的键必须指向同一份文件。
// 写成 "qrlogin/.mi.token" 时 runtime 会再加一次前缀 ⇒ `qrlogin/qrlogin/.mi.token`，
// bootstrap 与换票拿到两个不同 deviceId。
func TestQRTokenKeyIsShared(t *testing.T) {
	if got := qrOptionsTokenPath(); got != ".mi.token" {
		t.Fatalf("TokenPath = %q（期望 .mi.token；带 qrlogin/ 前缀会与 runtime 前缀叠加）", got)
	}
}
