package main

import (
	"os"
	"strings"
	"testing"
)

// stripGoComments 去掉整行注释，避免"注释里引用了旧写法"被断言误判。
// （本测试第一次跑就撞上了这个坑：代码注释里记录着旧的错误写法原文。）
func stripGoComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "//") ||
			strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*/") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// 重登后的重试必须使用**新会话**（2026-11-25 连续失败 40 次的直接原因）。
//
// 钉住的回归：原来是
//
//	_, newSSec, _, oerr := openCloudSession(ctx)
//	return apiPostRC4GuestDeviceOnce(ctx, sessionID, newSSec, ...)   ← 旧 sessionID
//
// 新会话带着 attach 好的新票据，却被丢弃；重试走旧会话 ⇒ 又一次 invalid signature。
// 日志证据：00:20:03.436 自动重登成功 → 00:20:03.514 sync 失败 invalid signature。
func TestReloginRetryUsesNewSession(t *testing.T) {
	src, err := os.ReadFile("rc4_api.go")
	if err != nil {
		t.Fatalf("读 rc4_api.go: %v", err)
	}
	code := stripGoComments(string(src))

	// 必须接住 openCloudSession 的第一个返回值（新会话 ID）
	if !strings.Contains(code, "newSess, newSSec, newDevID, oerr := openCloudSession(ctx)") {
		t.Fatal("重登后必须接收 openCloudSession 返回的新 sessionID（newSess）")
	}
	// 重试必须用它
	if !strings.Contains(code, "apiPostRC4GuestDeviceOnce(ctx, newSess, newSSec, newDevID, apiURL, data)") {
		t.Fatal("重登后的重试必须使用新会话 newSess，不能用旧 sessionID")
	}
	// 且必须关闭，避免撞宿主 4 会话上限
	if !strings.Contains(code, "_ = ctx.HTTPClose(newSess)") {
		t.Fatal("新会话用完必须关闭（宿主上限 4 个会话）")
	}
	// 不能再把新会话丢弃
	if strings.Contains(code, "_, newSSec, _, oerr := openCloudSession(ctx)") {
		t.Fatal("仍存在丢弃新 sessionID 的写法")
	}
}

// 会话 attach 必须逐 host 记录（诊断 invalid signature 两个成因的必要信息）。
func TestSessionAttachLogsPerHost(t *testing.T) {
	src, err := os.ReadFile("cloud_sync.go")
	if err != nil {
		t.Fatalf("读 cloud_sync.go: %v", err)
	}
	code := stripGoComments(string(src))
	if !strings.Contains(code, "hx-mi 会话 attach host=%s ok=%d missing=%v") {
		t.Fatal("openCloudSession 必须逐 host 记录 attach 数量与缺失项，否则 invalid signature 无法定位")
	}
}
