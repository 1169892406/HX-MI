package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseStep1JSONPFixture(t *testing.T) {
	v, err := parseAuth([]byte(`&&&START&&&{"_sign":"sig","qs":"%3Fsid%3Dxiaomiio","callback":"https://sts.api.io.mi.com/sts"}`))
	if err != nil || v.Sign != "sig" || v.QS == "" || v.Callback == "" {
		t.Fatalf("%#v %v", v, err)
	}
}
func TestStep2FixtureAndPasswordNotPersisted(t *testing.T) {
	p := protocolState{Sign: "sig", QS: "qs", Callback: "cb"}
	b := string(formBody("user@example.com", "P@ssword", p, "1234"))
	if strings.Contains(b, "P%40ssword") || strings.Contains(b, "P@ssword") {
		t.Fatal("password leaked")
	}
	if !strings.Contains(b, "hash=382E0360E4EB7B70034FBAA69BEC5786") || !strings.Contains(b, "captCode=1234") {
		t.Fatal(b)
	}
	raw, _ := json.Marshal(loginSession{MaskedUser: "u***@example.com", Protocol: p})
	if strings.Contains(string(raw), "P@ssword") {
		t.Fatal(string(raw))
	}
}
func TestCaptchaAndVerifyFixture(t *testing.T) {
	v, err := parseAuth([]byte(`{"code":70016,"captchaUrl":"/pass/getCode","notificationUrl":"https://account.xiaomi.com/identity/auth/verify"}`))
	if err != nil || absoluteXiaomiURL(v.CaptchaURL) != "https://account.xiaomi.com/pass/getCode" || !allowedVerifyURL(v.NotificationURL) {
		t.Fatalf("%#v %v", v, err)
	}
	if allowedVerifyURL("https://evil.example/") {
		t.Fatal("untrusted redirect accepted")
	}
}
func TestErrorAndMaskCompatibility(t *testing.T) {
	if got := explainLoginCode(70016, ""); !strings.Contains(got, "密码") {
		t.Fatal(got)
	}
	if got := maskUser("user@example.com"); got != "u***@example.com" {
		t.Fatal(got)
	}
	if got := maskUser("13800138000"); strings.Contains(got, "13800138000") {
		t.Fatal(got)
	}
}
func TestMalformedFixture(t *testing.T) {
	if _, err := parseAuth([]byte(`not-json`)); err == nil {
		t.Fatal("want error")
	}
	if got := stripJSONP([]byte(" &&&START&&& {\"code\":0} ")); string(got) != "{\"code\":0}" {
		t.Fatal(string(got))
	}
}
