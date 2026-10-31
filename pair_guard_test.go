package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

// TestExchangeAndProbeRejectsEmptySsecurity 钉住：ssecurity 为空时必须在换票前
// 就明确失败，而不是拿空串去签名。
//
// 事故背景（用户 00:39 实测）：step1 走"已有有效会话"分支、11 条票据全部 sealed，
// 探针仍回 {"code":0,"message":"invalid signature"}。该错误看起来像算法错，
// 实际可能只是没拿到钥匙 —— 两者必须能区分，否则诊断只能靠猜。
func TestExchangeAndProbeRejectsEmptySsecurity(t *testing.T) {
	h := hosttest.New(t)
	h.Fixture(plugin.OpHTTPSessionRequest, plugin.HTTPSessionResponse{Status: 200, BodyB64: base64.StdEncoding.EncodeToString([]byte("ok"))})
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	s := &loginSession{HostSessionID: "hs", Region: "cn", DeviceID: "dev"}

	_, err := exchangeAndProbe(ctx, "s1", s, "", "", "")
	if err == nil {
		t.Fatal("ssecurity 为空时不得继续探针")
	}
	if !strings.Contains(err.Error(), "会话密钥") {
		t.Fatalf("错误必须说清是没拿到钥匙，而不是签名失败: %v", err)
	}
	if strings.Contains(err.Error(), "invalid signature") {
		t.Fatalf("不该把空钥匙伪装成签名错误: %v", err)
	}
}

// TestExchangeAndProbeStopsOnEnvelopeError 钉住：小米拒绝时不得把状态置 completed。
func TestExchangeAndProbeStopsOnEnvelopeError(t *testing.T) {
	h := hosttest.New(t)
	h.Fixture(plugin.OpHTTPSessionRequest, plugin.HTTPSessionResponse{Status: 200, BodyB64: base64.StdEncoding.EncodeToString([]byte("ok"))})
	h.Fixture(plugin.OpHTTPCookiesSeal, map[string]any{"host": "api.io.mi.com", "sealed": []map[string]any{{"name": "serviceToken"}}})
	h.Fixture(plugin.OpHTTPCookiesAttach, map[string]any{"attached": 1})
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	s := &loginSession{HostSessionID: "hs", Region: "cn", DeviceID: "dev"}

	_, err := exchangeAndProbe(ctx, "s1", s, "", "QUFB", "")
	if err == nil {
		t.Fatal("探针失败必须返回错误")
	}
	if s.State == "completed" {
		t.Fatal("探针未通过绝不能置 completed")
	}
}

// TestAccountIdentityLeaksNoSecrets 钉住：展示用身份绝不带出凭据。
func TestAccountIdentityLeaksNoSecrets(t *testing.T) {
	saved, _ := json.Marshal(map[string]any{
		"version": 2, "account": "someone@example.com", "masked_user": "s***@example.com",
		"region": "cn", "saved_at": "2026-10-31T00:00:00Z", "tickets_sealed": true,
	})
	h := hosttest.New(t)
	h.On(plugin.OpFSRead, func(raw json.RawMessage) (any, error) {
		var r struct{ Path string `json:"path"` }
		_ = json.Unmarshal(raw, &r)
		if r.Path == "cloud-session.json" {
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(saved)}, nil
		}
		return map[string]any{"found": false}, nil
	})
	h.Fixture(plugin.OpVaultBlobOpen, map[string]any{"found": false})
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}

	id := loadAccountIdentity(ctx)
	if id == nil {
		t.Fatal("应读到身份")
	}
	// 用户明确要求显示**完整**账号，不要加 *
	if id.Account != "someone@example.com" {
		t.Fatalf("应返回完整账号，实际 %q", id.Account)
	}
	if strings.Contains(id.Account, "*") {
		t.Fatalf("完整账号里不应有脱敏星号: %q", id.Account)
	}
	// 脱敏值单独保留给日志面
	if id.Masked == "" || !strings.Contains(id.Masked, "*") {
		t.Fatalf("MaskedUser 应保持脱敏形态: %q", id.Masked)
	}
}

var _ = errors.New
