package main

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

func sessionFixture(t *testing.T, challenge *loginChallenge) *hosttest.Host {
	t.Helper()
	h := hosttest.New(t)
	sess, _ := json.Marshal(loginSession{
		HostSessionID: "hs-1", Region: "cn", State: "need_challenge",
		Protocol: protocolState{QS: "?sid=xiaomiio", Challenge: challenge},
	})
	h.On(plugin.OpFSRead, func(raw json.RawMessage) (any, error) {
		var r struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(raw, &r)
		if strings.HasPrefix(r.Path, "login-sessions/") {
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(sess)}, nil
		}
		return map[string]any{"found": false}, nil
	})
	h.Fixture(plugin.OpFSWrite, map[string]any{"ok": true})
	return h
}

func TestVerifyCodeRejectsEmpty(t *testing.T) {
	sessionFixture(t, &loginChallenge{Kind: "web_verify", VerifyURL: "https://account.xiaomi.com/fe/service/identity/authStart?sid=xiaomiio"})
	_, err := call(&plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"},
		"verify", json.RawMessage(`{"session_id":"s1","code":"  "}`))
	if err == nil || !strings.Contains(err.Error(), "验证码为空") {
		t.Fatalf("空验证码应明确：%v", err)
	}
}

func TestVerifyCodeWithoutVerifyURL(t *testing.T) {
	sessionFixture(t, nil)
	_, err := call(&plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"},
		"verify", json.RawMessage(`{"session_id":"s1","code":"123456"}`))
	if err == nil || !strings.Contains(err.Error(), "不需要") {
		t.Fatalf("无验证入口应明确：%v", err)
	}
}

// TestVerifyCodeSubmitsTicketToXiaomi 验证码确实被提交到小米 verifyPhone 接口，
// 而不是像修复前那样去读 url 字段并报“链接为空”。
func TestVerifyCodeSubmitsTicketToXiaomi(t *testing.T) {
	sessionFixture(t, &loginChallenge{
		Kind: "web_verify", VerifyURL: "https://account.xiaomi.com/fe/service/identity/authStart?sid=xiaomiio&context=x",
	})
	tickets := 0
	h := hosttest.New(t) // 覆盖上一步注册的宿主替身
	sess, _ := json.Marshal(loginSession{
		HostSessionID: "hs-1", Region: "cn", State: "need_challenge",
		Protocol: protocolState{QS: "?sid=xiaomiio", Challenge: &loginChallenge{
			Kind: "web_verify", VerifyURL: "https://account.xiaomi.com/fe/service/identity/authStart?sid=xiaomiio&context=x"}},
	})
	h.On(plugin.OpFSRead, func(raw json.RawMessage) (any, error) {
		var r struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(raw, &r)
		return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(sess)}, nil
	})
	h.Fixture(plugin.OpFSWrite, map[string]any{"ok": true})
	h.Fixture(plugin.OpHTTPInfo, map[string]any{"session_id": "hs-1", "active": true})
	h.On(plugin.OpHTTPSessionRequest, func(raw json.RawMessage) (any, error) {
		var req plugin.HTTPSessionRequest
		_ = json.Unmarshal(raw, &req)
		switch {
		case strings.Contains(req.URL, "identity/list"):
			return plugin.HTTPSessionResponse{Status: 200, BodyB64: base64.StdEncoding.EncodeToString([]byte(`{"code":2,"flag":4,"option":4,"options":[4],"version":"v2"}`))}, nil
		case strings.Contains(req.URL, "verifyPhone"):
			body, _ := base64.StdEncoding.DecodeString(req.BodyB64)
			q, _ := url.ParseQuery(string(body))
			if q.Get("ticket") == "246810" && q.Get("_flag") == "4" {
				tickets++
			}
			return plugin.HTTPSessionResponse{Status: 200, BodyB64: base64.StdEncoding.EncodeToString([]byte(`{"code":70016,"desc":"验证码错误"}`))}, nil
		}
		t.Fatalf("意外请求 %s", req.URL)
		return nil, nil
	})
	_, err := call(&plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"},
		"verify", json.RawMessage(`{"session_id":"s1","code":"246810"}`))
	if tickets != 1 {
		t.Fatalf("验证码未提交到 verifyPhone（tickets=%d）", tickets)
	}
	if err == nil || !strings.Contains(err.Error(), "验证码未通过") {
		t.Fatalf("小米拒绝应如实返回：%v", err)
	}
}

func TestVerifyWebEmptyURLIsExplicit(t *testing.T) {
	sessionFixture(t, &loginChallenge{Kind: "web_verify", VerifyURL: "https://account.xiaomi.com/x"})
	_, err := call(&plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"},
		"verify_web", json.RawMessage(`{"session_id":"s1","url":""}`))
	if err == nil || !strings.Contains(err.Error(), "链接为空") {
		t.Fatalf("空链接应明确：%v", err)
	}
}

func TestVerifyCodeStaleHostSession(t *testing.T) {
	// HTTPInfo 未注册 → 宿主返回“未实现”，插件应提示重新登录而不是继续请求。
	sessionFixture(t, &loginChallenge{Kind: "web_verify", VerifyURL: "https://account.xiaomi.com/fe/service/identity/authStart?sid=xiaomiio"})
	_, err := call(&plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"},
		"verify", json.RawMessage(`{"session_id":"s1","code":"123456"}`))
	if err == nil || !strings.Contains(err.Error(), "登录会话已失效") {
		t.Fatalf("过期会话应明确提示重新登录：%v", err)
	}
}
