// 账号存储密钥与会话解耦（2026-11-10 用户实测「我原来的账号信息哪里去了」）。
//
// # 事故形态
//
// 用户点「退出登录」后，配置页整块报
//
//	PLUGIN_CALL_FAILED: 账号主密钥不可用（accounts_master_key 未配置，且会话材料不存在）
//
// 账号列表读不出来，看起来像数据没了（实际 accounts.json 一直在磁盘上）。
//
// # 两处真身（各一条闸门）
//
//	① accounts.json 是**明文** JSON，密钥只用于 accounts.secrets/<id>.enc；
//	   但 openAccountStore 在密钥缺席时直接返回错误，把"读列表"也挡死了。
//	② 密钥的兜底来源是会话材料，而 logout 正会删它 ⇒ 退出登录 = 密钥永久丢失。
//
// 这里判的是**源码结构**：真正的行为要宿主 Vault 才能跑，而这两条改动的
// 关键恰恰是"哪个函数在什么情况下返回错误"—— 读源码能钉死，且不会因为
// 未来某个 mock 的松散实现而假绿。
package main

import (
	"os"
	"strings"
	"testing"
)

func readSrc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("读 %s: %v", name, err)
	}
	return string(b)
}

// ① 密钥缺席不得阻断账号列表。
func TestAccountStoreOpensWithoutKey(t *testing.T) {
	src := readSrc(t, "account_store.go")

	// openAccountStore 里不能再有 return nil, errors.New(...) 这种"没密钥就整块失败"。
	start := strings.Index(src, "func openAccountStore(")
	end := strings.Index(src, "// deriveAccountKey")
	if start < 0 || end < 0 || end < start {
		t.Fatal("找不到 openAccountStore 的实现区间")
	}
	body := src[start:end]
	if strings.Contains(body, "return nil, errors.New") {
		t.Fatal("openAccountStore 在密钥不可用时不得整块失败：accounts.json 是明文元数据，" +
			"把它一起挡死会让用户以为账号数据丢了（实际一直还在）")
	}
	if !strings.Contains(body, "return &accountStore{ctx: ctx}, nil") {
		t.Fatal("密钥不可用时应返回**可用但无密钥**的 store（列表可读，凭据加解密才报错）")
	}

	// 专用密钥材料必须与会话解耦：用一个不会被 logout 删除的独立 blob。
	if !strings.Contains(src, `accountsKeyBlob = "accounts_key_material"`) {
		t.Fatal("密钥必须派生自专用且永不删除的 blob（accounts_key_material）")
	}
}

// ② logout 不得删除账号存储的密钥材料。
func TestLogoutKeepsAccountKey(t *testing.T) {
	src := readSrc(t, "cloud_tools.go")
	if !strings.Contains(src, `VaultBlobDelete("xiaomi_session_material")`) {
		t.Fatal("logout 应当清掉会话材料（这是它的职责）")
	}
	if strings.Contains(src, `VaultBlobDelete("accounts_key_material")`) ||
		strings.Contains(src, "accountsKeyBlob") && strings.Contains(src, "VaultBlobDelete(accountsKeyBlob") {
		t.Fatal("logout 绝不能删账号存储的密钥材料：那等于「退出登录 = 吊销自己的账号库」")
	}
}

// ③ 会话密钥不存在是正常状态，不得当故障上抛。
func TestMissingSessionSecretIsNotAnError(t *testing.T) {
	src := readSrc(t, "token_exchange.go")
	idx := strings.Index(src, "secretBytes, _, e := ctx.VaultBlobOpen(saved.SecretRef)")
	if idx < 0 {
		t.Fatal("找不到会话密钥读取点")
	}
	seg := src[idx:]
	if end := strings.Index(seg, "hs, err := ctx.HTTPOpen"); end > 0 {
		seg = seg[:end]
	}
	if !strings.Contains(seg, "plugin.CodeNotFound") || !strings.Contains(seg, `return "no_session", nil`) {
		t.Fatal("vault blob 不存在（典型：用户刚点过退出登录）必须回 no_session 而不是报错 —— " +
			"否则「没登录」这个最正常的状态会让同步结果卡整块报错")
	}
}
