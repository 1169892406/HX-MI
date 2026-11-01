package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

// TestLoginNeverReusesExistingTickets 钉住：登录入口不得复用上一个账号的票据。
//
// 事故（用户多次实测 invalid signature）：已登录账号 A 时去添加账号 B，
// canReuse 把 A 的 serviceToken attach 进新会话；小米 step1 因此认为"已有有效
// 会话"，返回 code=0 sign=false（不给 _sign，不走账号密码流程），插件就用 A 的
// 材料去探针 —— 小米回 invalid signature。日志特征：票据 3+4+4=11 条全 sealed。
//
// 真实账号实验：干净 jar 下 step1 必回 code=70016 sign=true（可走账号密码）。
func TestLoginNeverReusesExistingTickets(t *testing.T) {
	h := hosttest.New(t)
	h.Fixture(plugin.OpFSList, map[string]any{"files": []map[string]any{}})
	h.Fixture(plugin.OpHTTPOpen, map[string]any{"session_id": "hs-new"})
	// 模拟"已登录过"：cloud-session.json 存在且票据已密封（旧代码会因此 canReuse）
	saved, _ := json.Marshal(map[string]any{
		"version": 2, "tickets_sealed": true, "secret_ref": "xiaomi_session_material",
	})
	h.On(plugin.OpFSRead, func(raw json.RawMessage) (any, error) {
		var r struct{ Path string `json:"path"` }
		_ = json.Unmarshal(raw, &r)
		switch r.Path {
		case "cloud-session.json":
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(saved)}, nil
		case "device_id":
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString([]byte("dev1"))}, nil
		}
		return map[string]any{"found": false}, nil
	})

	attached := 0
	h.On(plugin.OpHTTPCookiesAttach, func(raw json.RawMessage) (any, error) {
		attached++
		return map[string]any{"attached": []map[string]any{{"name": "serviceToken"}}}, nil
	})
	deleted := 0
	h.On(plugin.OpHTTPCookiesDelete, func(raw json.RawMessage) (any, error) {
		deleted++
		return map[string]any{"deleted": 1}, nil
	})
	h.Fixture(plugin.OpHTTPClose, map[string]any{"ok": true})
	// step1 直接失败（本测试只关心票据处理方式，不关心后续流程）
	h.Fixture(plugin.OpHTTPSessionRequest, plugin.HTTPSessionResponse{
		Status: 200, BodyB64: base64.StdEncoding.EncodeToString([]byte(`&&&START&&&{"code":70016,"_sign":"s","qs":"?q"}&&&END&&&`)),
	})

	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	_, _ = startLogin(ctx, json.RawMessage(`{"user":"b@example.com","password":"x","region":"cn"}`))

	if attached != 0 {
		t.Fatalf("登录入口不得 attach 旧票据（实际 attach %d 次）——会把小米引到\"已有会话\"分支并串号", attached)
	}
	if deleted < 3 {
		t.Fatalf("必须先清掉三个 host 的旧票据，实际只删了 %d 次", deleted)
	}
}
