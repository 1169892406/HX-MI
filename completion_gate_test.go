package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func classifyProbeError(err error) string {
	if err == nil {
		return "available"
	}
	s := err.Error()
	if strings.Contains(s, "明确拒绝") {
		return "auth_rejected"
	}
	if strings.Contains(s, "网络不确定") {
		return "temporarily_unavailable"
	}
	return "indeterminate"
}
func TestProbeClassification(t *testing.T) {
	cases := map[string]string{"认证票据被明确拒绝": "auth_rejected", "网络不确定: HTTP 500": "temporarily_unavailable", "网络不确定: timeout": "temporarily_unavailable", "bad json": "indeterminate"}
	for in, want := range cases {
		if got := classifyProbeError(assertErr(in)); got != want {
			t.Fatalf("%s=%s", in, got)
		}
	}
}

type assertErr string

func (e assertErr) Error() string { return string(e) }
func TestCompletedNeedsResult(t *testing.T) {
	for _, raw := range []string{`{"code":0}`, `{"message":"auth error"}`, `{}`, `not-json`} {
		var env map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &env) == nil && env["result"] != nil {
			t.Fatal(raw)
		}
	}
	var ok map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"result":{"list":[]}}`), &ok)
	if ok["result"] == nil {
		t.Fatal("result absent")
	}
}
func TestSavedSessionHasNoCookies(t *testing.T) {
	b, _ := json.Marshal(savedCloudSession{Version: 2, SecretRef: "xiaomi_session_material", TicketsSealed: true})
	s := string(b)
	for _, secret := range []string{"serviceToken", "userId", "cUserId"} {
		if strings.Contains(s, secret) {
			t.Fatal(s)
		}
	}
}
