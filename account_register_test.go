package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

// TestLoginRegistersAccountIntoStore 钉住：登录成功后账号必须进入多账号库。
//
// 事故（用户："添加新账号后已经登录账号不显示" + 反复添加失败）：login/verify
// 把凭据写进 cloud-session.json 就返回，account_store 从未被写过 ——
// accounts 工具永远返回空数组，前端 v2 分支不接管，账号加不进去。
func TestLoginRegistersAccountIntoStore(t *testing.T) {
	h := hosttest.New(t)
	files := map[string][]byte{}
	h.On(plugin.OpFSRead, func(raw json.RawMessage) (any, error) {
		var r struct{ Path string `json:"path"` }
		_ = json.Unmarshal(raw, &r)
		if b, ok := files[r.Path]; ok {
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(b)}, nil
		}
		return map[string]any{"found": false}, nil
	})
	h.On(plugin.OpFSWrite, func(raw json.RawMessage) (any, error) {
		var w plugin.FSWriteRequest
		_ = json.Unmarshal(raw, &w)
		if b, err := base64.StdEncoding.DecodeString(w.DataB64); err == nil {
			files[w.Path] = b
		}
		return map[string]any{"ok": true}, nil
	})
	// 主密钥缺席 → 走保险库派生兜底
	h.Fixture(plugin.OpSecretGet, map[string]any{"value": ""})
	sec, _ := json.Marshal(cloudSecretMaterial{Ssecurity: "QUFB", PassToken: "pt"})
	h.Fixture(plugin.OpVaultBlobOpen, map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(sec)})

	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	s := &loginSession{Account: "someone@example.com", MaskedUser: "s***@example.com", Region: "cn"}
	secret := []byte(`{"ssecurity":"QUFB"}`)
	if err := registerAccountAfterLogin(ctx, s, secret); err != nil {
		t.Fatalf("登记账号失败: %v", err)
	}

	raw, ok := files["accounts.json"]
	if !ok {
		t.Fatal("未写入 accounts.json —— 账号加不进多账号库")
	}
	var env accountEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("accounts.json 不合法: %v", err)
	}
	if len(env.Accounts) != 1 {
		t.Fatalf("应有 1 个账号，实际 %d", len(env.Accounts))
	}
	if env.Accounts[0].CloudIdentity != "someone@example.com" {
		t.Fatalf("账号身份错误: %q", env.Accounts[0].CloudIdentity)
	}
	if env.Accounts[0].State != "ready_unbound" {
		t.Fatalf("新账号应为 ready_unbound，实际 %q", env.Accounts[0].State)
	}
	// 秘密必须落盘（否则账号不可用）
	if len(files["accounts.secrets/"+env.Accounts[0].AccountID+".enc"]) == 0 {
		t.Fatal("账号秘密未落盘")
	}
}

// TestRegisterIsIdempotent 同一账号重复登录只更新、不新增。
func TestRegisterIsIdempotent(t *testing.T) {
	h := hosttest.New(t)
	files := map[string][]byte{}
	h.On(plugin.OpFSRead, func(raw json.RawMessage) (any, error) {
		var r struct{ Path string `json:"path"` }
		_ = json.Unmarshal(raw, &r)
		if b, ok := files[r.Path]; ok {
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(b)}, nil
		}
		return map[string]any{"found": false}, nil
	})
	h.On(plugin.OpFSWrite, func(raw json.RawMessage) (any, error) {
		var w plugin.FSWriteRequest
		_ = json.Unmarshal(raw, &w)
		if b, err := base64.StdEncoding.DecodeString(w.DataB64); err == nil {
			files[w.Path] = b
		}
		return map[string]any{"ok": true}, nil
	})
	h.Fixture(plugin.OpSecretGet, map[string]any{"value": ""})
	sec, _ := json.Marshal(cloudSecretMaterial{Ssecurity: "QUFB"})
	h.Fixture(plugin.OpVaultBlobOpen, map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(sec)})

	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	s := &loginSession{Account: "dup@example.com", MaskedUser: "d***@example.com", Region: "cn"}
	secret := []byte(`{"ssecurity":"QUFB"}`)
	if err := registerAccountAfterLogin(ctx, s, secret); err != nil {
		t.Fatal(err)
	}
	if err := registerAccountAfterLogin(ctx, s, secret); err != nil {
		t.Fatal(err)
	}
	var env accountEnvelope
	json.Unmarshal(files["accounts.json"], &env)
	if len(env.Accounts) != 1 {
		t.Fatalf("重复登录不应新增账号，实际 %d 个", len(env.Accounts))
	}
	if env.Accounts[0].Revision < 2 {
		t.Fatalf("重复登录应递增 revision，实际 %d", env.Accounts[0].Revision)
	}
}

// TestAccountSyncNotStub 钉住 account_sync 不再返回假 queued。
func TestAccountSyncNotStub(t *testing.T) {
	b, _ := mustReadFile(t, "main.go")
	if strings.Contains(b, `return json.Marshal(map[string]any{"state": "queued"})`) {
		t.Fatal("account_sync 仍是空壳实现（直接返回 queued，什么都不做）")
	}
}
