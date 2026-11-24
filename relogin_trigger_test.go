package main

import (
	"errors"
	"testing"
)

// invalid signature 必须触发自动重登（2026-11-25 用户「连续失败 37 次」）。
//
// 钉住的回归：isServiceTokenExpired 原来不认 `invalid signature`，
// 于是每次同步都判"不是票据问题"⇒ 永不重登 ⇒ 一路错到 37 次。
func TestIsServiceTokenExpired_InvalidSignature(t *testing.T) {
	err := errors.New("小米业务接口拒绝（code=0，说明：invalid signature）")
	if !isServiceTokenExpired(err) {
		t.Fatal("invalid signature 必须判定为票据失效并触发重登 —— 否则永远不自愈")
	}
}

// 票据类错误都要认（历史形态）。
func TestIsServiceTokenExpired_KnownForms(t *testing.T) {
	cases := []string{
		"SERVICETOKEN_EXPIRED",
		"service_token invalid",
		"AUTH_ERR",
		"auth error",
		"登录已过期",
		"小米业务接口拒绝（code=0，说明：invalid signature）",
	}
	for _, c := range cases {
		if !isServiceTokenExpired(errors.New(c)) {
			t.Errorf("%q 应判定为票据失效", c)
		}
	}
}

// 普通网络错误不能被误判成票据问题（否则会无谓地反复重登）。
func TestIsServiceTokenExpired_NotForOtherErrors(t *testing.T) {
	cases := []string{
		"context deadline exceeded",
		"dial tcp: connection refused",
		"设备清单响应不确定",
	}
	for _, c := range cases {
		if isServiceTokenExpired(errors.New(c)) {
			t.Errorf("%q 不该被误判为票据失效", c)
		}
	}
	if isServiceTokenExpired(nil) {
		t.Error("nil 错误不该判定为票据失效")
	}
}
