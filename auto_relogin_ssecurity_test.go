package main

import "testing"

// ssecurity 必须从**本次换票的 location** 取，绝不复用旧值（2026-11-24 invalid signature 根因）。
func TestSsecurityFromLocation(t *testing.T) {
	cases := []struct {
		name string
		loc  string
		want string
	}{
		{"标准查询参数", "https://sts.api.io.mi.com/sts?sid=xiaomiio&ssecurity=AbC123&passToken=xyz", "AbC123"},
		{"多参数乱序", "https://sts.api.io.mi.com/sts?passToken=xyz&ssecurity=QqWwEe&sid=xiaomiio", "QqWwEe"},
		{"URL 编码的值", "https://sts.api.io.mi.com/sts?ssecurity=a%2Bb%3Dc", "a+b=c"},
		{"fragment 里", "https://account.xiaomi.com/pass#?ssecurity=FragSec&x=1", "FragSec"},
		{"没有 ssecurity", "https://sts.api.io.mi.com/sts?sid=xiaomiio", ""},
		{"空串", "", ""},
		{"非法 URL", "://bad", ""},
	}
	for _, c := range cases {
		if got := ssecurityFromLocation(c.loc); got != c.want {
			t.Errorf("%s: ssecurityFromLocation(%q) = %q, want %q", c.name, c.loc, got, c.want)
		}
	}
}

// 关键回归：location 里**没有** ssecurity 时必须返回空，
// 让调用方明确失败 —— 绝不能"回落到旧值"把新 token 配上旧密钥。
func TestSsecurityFromLocation_NeverGuesses(t *testing.T) {
	if got := ssecurityFromLocation("https://sts.api.io.mi.com/sts?sid=xiaomiio&passToken=pt"); got != "" {
		t.Fatalf("location 缺 ssecurity 时必须返回空串（让调用方拒绝复用旧值），得到 %q", got)
	}
}
