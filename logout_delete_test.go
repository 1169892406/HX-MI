package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

// TestLogoutDeletesAllCredentials 钉住"账号删得掉"。
//
// 事故（用户实测"账号删除不掉"）：旧 logoutLogin 只在传了 session_id 时删
// 登录会话文件，cloud-session.json / 保险库密钥 / 已密封票据全都不碰 ——
// 于是点删除后 account 工具仍读到已登录态，账号行照旧出现。
func TestLogoutDeletesAllCredentials(t *testing.T) {
	h := hosttest.New(t)
	h.Fixture(plugin.OpFSList, map[string]any{"files": []map[string]any{
		{"path": "login-sessions/a.json", "size": 10},
	}})
	removed := map[string]bool{}
	h.On(plugin.OpFSRemove, func(raw json.RawMessage) (any, error) {
		var r struct{ Path string `json:"path"` }
		_ = json.Unmarshal(raw, &r)
		removed[r.Path] = true
		return map[string]any{"ok": true}, nil
	})
	// 删除后回读必须找不到
	h.Fixture(plugin.OpFSRead, map[string]any{"found": false})
	vaultDeleted := false
	h.On(plugin.OpVaultBlobDelete, func(raw json.RawMessage) (any, error) {
		vaultDeleted = true
		return map[string]any{"deleted": true}, nil
	})
	cookiesDeleted := 0
	h.On(plugin.OpHTTPCookiesDelete, func(raw json.RawMessage) (any, error) {
		cookiesDeleted++
		return map[string]any{"deleted": 1}, nil
	})

	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	out, err := logoutLogin(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if !strings.Contains(string(out), "deleted") {
		t.Fatalf("应返回 deleted，实际 %s", out)
	}
	// ① 存在的证据
	if !removed["cloud-session.json"] {
		t.Error("必须删除 cloud-session.json（否则 account 仍报已登录）")
	}
	// ② 保险库密钥
	if !vaultDeleted {
		t.Error("必须删除保险库里的会话密钥")
	}
	// ③ 已密封票据（三个 host）
	if cookiesDeleted < 3 {
		t.Errorf("必须清理三个 host 的票据，实际 %d", cookiesDeleted)
	}
	// ④ 同步状态快照
	if !removed[syncStatePath] {
		t.Error("必须删除同步状态快照（账号没了，数字再留着就是谎）")
	}
}

// TestLogoutFailsLoudlyOnPartialFailure 钉住：删不干净必须报错，不能假装成功。
func TestLogoutFailsLoudlyOnPartialFailure(t *testing.T) {
	h := hosttest.New(t)
	h.Fixture(plugin.OpFSList, map[string]any{"files": []map[string]any{}})
	h.Fixture(plugin.OpFSRemove, map[string]any{"ok": true})
	h.Fixture(plugin.OpVaultBlobDelete, map[string]any{"deleted": true})
	h.Fixture(plugin.OpHTTPCookiesDelete, map[string]any{"deleted": 1})
	// 删完回读仍然存在 → 说明没删掉
	h.Fixture(plugin.OpFSRead, map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString([]byte(`{}`))})

	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	_, err := logoutLogin(ctx, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("会话文件仍存在时必须报错，不能假装删除成功")
	}
}
