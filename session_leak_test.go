package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

// TestReapClosesCompletedSessions 钉住会话不泄漏：
// 已完成/已失败的登录会话必须被关闭并删除，否则宿主 4 个会话上限会被耗光
// （用户实测：反复"退出重新登录"后 PLUGIN_RESOURCE_EXHAUSTED）。
func TestReapClosesCompletedSessions(t *testing.T) {
	h := hosttest.New(t)
	completed, _ := json.Marshal(loginSession{HostSessionID: "hs-done", State: "completed"})
	failed, _ := json.Marshal(loginSession{HostSessionID: "hs-fail", State: "failed"})
	challenge, _ := json.Marshal(loginSession{HostSessionID: "hs-chal", State: "need_challenge"})

	h.Fixture(plugin.OpFSList, map[string]any{"files": []map[string]any{
		{"path": "login-sessions/done.json", "size": 10},
		{"path": "login-sessions/fail.json", "size": 10},
		{"path": "login-sessions/chal.json", "size": 10},
		{"path": "devices.json", "size": 10},
	}})
	h.On(plugin.OpFSRead, func(raw json.RawMessage) (any, error) {
		var r struct{ Path string `json:"path"` }
		_ = json.Unmarshal(raw, &r)
		switch r.Path {
		case "login-sessions/done.json":
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(completed)}, nil
		case "login-sessions/fail.json":
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(failed)}, nil
		case "login-sessions/chal.json":
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(challenge)}, nil
		}
		return map[string]any{"found": false}, nil
	})

	closed := map[string]bool{}
	removed := map[string]bool{}
	h.On(plugin.OpHTTPClose, func(raw json.RawMessage) (any, error) {
		var r struct{ SessionID string `json:"session_id"` }
		_ = json.Unmarshal(raw, &r)
		closed[r.SessionID] = true
		return map[string]any{"ok": true}, nil
	})
	h.On(plugin.OpFSRemove, func(raw json.RawMessage) (any, error) {
		var r struct{ Path string `json:"path"` }
		_ = json.Unmarshal(raw, &r)
		removed[r.Path] = true
		return map[string]any{"ok": true}, nil
	})

	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	reapStaleLoginSessions(ctx, "")

	for _, id := range []string{"hs-done", "hs-fail"} {
		if !closed[id] {
			t.Errorf("会话 %s 应被关闭（否则泄漏）", id)
		}
	}
	for _, p := range []string{"login-sessions/done.json", "login-sessions/fail.json"} {
		if !removed[p] {
			t.Errorf("文件 %s 应被删除", p)
		}
	}
	// 唯一的挑战会话必须保留：用户可能正在输验证码
	if closed["hs-chal"] {
		t.Error("唯一的挑战会话不该被关闭")
	}
	if removed["login-sessions/chal.json"] {
		t.Error("唯一的挑战会话文件不该被删除")
	}
}

// TestStartLoginClosesSessionOnSuccess 钉住完成态必须关会话。
func TestStartLoginClosesSessionOnSuccess(t *testing.T) {
	h := hosttest.New(t)
	h.Fixture(plugin.OpFSList, map[string]any{"files": []map[string]any{}})
	h.Fixture(plugin.OpHTTPOpen, map[string]any{"session_id": "hs-new"})
	h.Fixture(plugin.OpHTTPCookiesAttach, map[string]any{"attached": 0})
	h.Fixture(plugin.OpHTTPCookiesDelete, map[string]any{"ok": true})
	h.Fixture(plugin.OpHTTPCookiesSeal, map[string]any{"sealed": []map[string]any{{"name": "serviceToken"}}})
	h.Fixture(plugin.OpVaultBlobSeal, map[string]any{"key": "xiaomi_session_material", "version": 1})
	h.Fixture(plugin.OpFSWrite, map[string]any{"ok": true})
	h.On(plugin.OpFSRead, func(raw json.RawMessage) (any, error) {
		var r struct{ Path string `json:"path"` }
		_ = json.Unmarshal(raw, &r)
		if r.Path == "device_id" {
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString([]byte("dev1"))}, nil
		}
		return map[string]any{"found": false}, nil
	})
	closedAny := false
	h.On(plugin.OpHTTPClose, func(raw json.RawMessage) (any, error) {
		closedAny = true
		return map[string]any{"ok": true}, nil
	})
	// step1 直接给 location（已认证会话）→ 走完成路径
	h.Fixture(plugin.OpHTTPSessionRequest, plugin.HTTPSessionResponse{
		Status:  200,
		BodyB64: base64.StdEncoding.EncodeToString([]byte(`&&&START&&&{"code":0,"ssecurity":"QUFB","location":"https://sts.api.io.mi.com/sts?d=1"}&&&END&&&`)),
	})

	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	_, _ = startLogin(ctx, json.RawMessage(`{"user":"a@b.com","password":"x","region":"cn"}`))
	if !closedAny {
		t.Fatal("登录结束必须关闭宿主会话，否则每次登录漏一个（上限 4）")
	}
}
