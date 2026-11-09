// manifest 的 scope 一致性（2026-11-10 用户实测「读取会话密钥失败」）。
//
// # 事故形态
//
// 点同步条目的「手动同步」→
//
//	同步失败：读取会话密钥失败：PLUGIN_PERMISSION_DENIED:
//	  家庭级 secret blob "xiaomi_session_material" 不接受账号上下文
//
// # 真身
//
// 平台按 **manifest 声明的 scope** 判定两件事，而这两件事必须自洽：
//   · 工具 scope=account ⇒ 调用被盖成"账号上下文"（CallAs → CallAccount）；
//   · blob scope=household ⇒ `blobAccount` 拒绝任何账号上下文。
// 于是"账号级工具去读家庭级 blob"必然被拒 —— 声明自相矛盾。
//
// 另一处：账号存储的密钥材料 blob `accounts_key_material` 此前**根本没声明**，
// 而 `blobRuleFor` 对未声明的 key 直接报错 ⇒ 取密钥永远失败
// （表现只是"凭据存不进去"，很容易被忽略）。
//
// 这两条都只有跑起来才暴露，而单测跑不到平台策略 —— 所以在**源码/清单层**钉死。
package main

import (
	"encoding/json"
	"os"
	"testing"
)

type manifestForTest struct {
	Resources struct {
		Secrets struct {
			Blobs []struct {
				Key   string `json:"key"`
				Scope string `json:"scope"`
			} `json:"blobs"`
		} `json:"secrets"`
	} `json:"resources"`
	Provides struct {
		Tools []struct {
			Name  string `json:"name"`
			Scope string `json:"scope"`
		} `json:"tools"`
	} `json:"provides"`
}

func loadManifest(t *testing.T) manifestForTest {
	t.Helper()
	b, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatalf("读 manifest.json: %v", err)
	}
	var m manifestForTest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("解析 manifest.json: %v", err)
	}
	return m
}

// ① 代码里用到的 VaultBlob key 必须在 manifest 声明（否则平台直接拒绝）。
func TestVaultBlobKeysAreDeclared(t *testing.T) {
	m := loadManifest(t)
	declared := map[string]string{}
	for _, b := range m.Resources.Secrets.Blobs {
		declared[b.Key] = b.Scope
	}
	// 与代码里的字面量保持一致（新增 key 时这里也要加，忘记即失败）。
	for _, key := range []string{"xiaomi_session_material", "accounts_key_material"} {
		if _, ok := declared[key]; !ok {
			t.Fatalf("VaultBlob %q 未在 manifest.resources.secrets.blobs 声明 —— "+
				"平台 blobRuleFor 会直接报「未在 manifest 声明」，取密钥必然失败", key)
		}
	}
	// 账号存储的密钥材料必须是家庭级：账号级工具也会用它（惰性取密钥），
	// 声明成 account 会让家庭级调用读不到。
	if declared["accounts_key_material"] != "household" {
		t.Fatalf("accounts_key_material 应为 household scope，实际 %q", declared["accounts_key_material"])
	}
}

// ② 工具 scope 与它用到的 blob scope 必须自洽。
func TestToolScopeMatchesBlobScope(t *testing.T) {
	src := readSrc(t, "cloud_tools.go")
	// 读会话材料的那个工具若被声明成账号级，平台会盖账号上下文 ⇒ 家庭级 blob 被拒。
	if !contains(src, `VaultBlobOpen("xiaomi_session_material")`) &&
		!contains(readSrc(t, "token_exchange.go"), `VaultBlobOpen(saved.SecretRef)`) {
		t.Fatal("找不到会话材料读取点（本判据的前提变了，请同步更新）")
	}
	// token_exchange.go 是 login/account_sync 共用的恢复路径 → 会话材料在家庭级上下文被读。
	for _, tool := range loadManifest(t).Provides.Tools {
		if tool.Name == "account_sync" && tool.Scope == "account" {
			t.Fatal("account_sync 不得声明为账号级：它要读**家庭级**会话材料，" +
				"账号上下文会被 blobAccount 拒绝（用户实测的「不接受账号上下文」）")
		}
	}
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
