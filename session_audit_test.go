package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

// TestRestoreClosesSessionOnEveryPath 钉住 restoreCloudSession 必须关会话。
//
// 事故（用户实测 PLUGIN_RESOURCE_EXHAUSTED「http 会话数超过上限 4」）：
// account 工具每次调用都走 restoreCloudSession，它开了会话却六条 return
// 路径全都没关；前端一挂载就轮询 account，几次就把宿主 4 个额度耗光。
// 本测试遍历所有退出路径，任何一条没关都会失败。
func TestRestoreClosesSessionOnEveryPath(t *testing.T) {
	// 只枚举**已经开了会话**的退出路径：无会话文件 / 票据未密封这两条在
	// HTTPOpen 之前就 return 了，本来就没有会话可关，断言它们"没关"是错的。
	cases := []struct {
		name  string
		setup func(h *hosttest.Host)
	}{
		{"无可用票据", func(h *hosttest.Host) {
			b, _ := json.Marshal(map[string]any{"version": 2, "tickets_sealed": true, "secret_ref": "xiaomi_session_material"})
			h.Fixture(plugin.OpFSRead, map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(b)})
			sec, _ := json.Marshal(cloudSecretMaterial{Ssecurity: "QUFB"})
			h.Fixture(plugin.OpVaultBlobOpen, map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(sec)})
			h.Fixture(plugin.OpHTTPOpen, map[string]any{"session_id": "hs-r"})
			h.Fixture(plugin.OpHTTPCookiesAttach, map[string]any{"attached": []map[string]any{}})
		}},
		{"探针成功", func(h *hosttest.Host) {
			b, _ := json.Marshal(map[string]any{"version": 2, "tickets_sealed": true, "secret_ref": "xiaomi_session_material"})
			h.Fixture(plugin.OpFSRead, map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(b)})
			sec, _ := json.Marshal(cloudSecretMaterial{Ssecurity: "QUFB"})
			h.Fixture(plugin.OpVaultBlobOpen, map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(sec)})
			h.Fixture(plugin.OpHTTPOpen, map[string]any{"session_id": "hs-r"})
			h.Fixture(plugin.OpHTTPCookiesAttach, map[string]any{"attached": []map[string]any{{"name": "serviceToken"}}})
			// 探针回错误信封 → 走 indeterminate 分支，同样必须关
			h.Fixture(plugin.OpHTTPSessionRequest, plugin.HTTPSessionResponse{
				Status: 200, BodyB64: base64.StdEncoding.EncodeToString([]byte("not-json")),
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hosttest.New(t)
			tc.setup(h)
			closed := false
			h.On(plugin.OpHTTPClose, func(raw json.RawMessage) (any, error) {
				closed = true
				return map[string]any{"ok": true}, nil
			})
			ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
			_, _ = restoreCloudSession(ctx)
			if !closed {
				t.Fatal("这条退出路径没有关闭宿主会话 —— 每次调用漏一个，4 次后登录不可用")
			}
		})
	}
}
