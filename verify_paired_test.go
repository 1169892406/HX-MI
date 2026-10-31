package main

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"testing"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

// TestVerifyCodeRefreshesPairedLocation 钉住 invalid-signature 的根因修复：
// 验证通过后 step1 返回 (ssecurity, location) 时，必须 GET 该 location
// 让 jar 收下**配对的** serviceToken，再探针。顺序：
//
//	verify location → serviceLogin(step1) → step1 的 location → device_list
func TestVerifyCodeRefreshesPairedLocation(t *testing.T) {
	h := hosttest.New(t)
	var mu sync.Mutex
	order := []string{}
	sess, _ := json.Marshal(loginSession{
		HostSessionID: "hs-1", Region: "cn", State: "need_challenge", DeviceID: "dev1",
		Protocol: protocolState{QS: "?sid=xiaomiio", Challenge: &loginChallenge{
			Kind: "web_verify", VerifyURL: "https://account.xiaomi.com/fe/service/identity/authStart?sid=xiaomiio"}},
	})
	h.On(plugin.OpFSRead, func(raw json.RawMessage) (any, error) {
		var r struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(raw, &r)
		if strings.HasPrefix(r.Path, "login-sessions/") {
			return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(sess)}, nil
		}
		return map[string]any{"found": false}, nil
	})
	h.Fixture(plugin.OpFSWrite, map[string]any{"ok": true})
	h.Fixture(plugin.OpHTTPInfo, map[string]any{"session_id": "hs-1", "active": true})
	h.Fixture(plugin.OpVaultBlobSeal, map[string]any{"key": "xiaomi_session_material", "present": true, "version": 1})
	h.Fixture(plugin.OpHTTPCookiesSeal, map[string]any{"host": "api.io.mi.com", "sealed": []map[string]any{{"name": "serviceToken"}}})
	pairedLoc := "https://sts.api.io.mi.com/sts?d=paired"
	h.On(plugin.OpHTTPSessionRequest, func(raw json.RawMessage) (any, error) {
		var req plugin.HTTPSessionRequest
		_ = json.Unmarshal(raw, &req)
		mu.Lock()
		order = append(order, req.URL)
		mu.Unlock()
		switch {
		case strings.Contains(req.URL, "identity/list"):
			return plugin.HTTPSessionResponse{Status: 200, BodyB64: base64.StdEncoding.EncodeToString([]byte(`&&&START&&&{"code":2,"options":[4]}&&&END&&&`))}, nil
		case strings.Contains(req.URL, "verifyPhone"):
			return plugin.HTTPSessionResponse{Status: 200, BodyB64: base64.StdEncoding.EncodeToString([]byte(`{"code":0,"location":"https://sts.api.io.mi.com/sts?d=verify"}`))}, nil
		case strings.Contains(req.URL, "serviceLogin"):
			// step1 返回配对材料：ssecurity + 新 location
			return plugin.HTTPSessionResponse{Status: 200, BodyB64: base64.StdEncoding.EncodeToString([]byte(
				`{"code":0,"ssecurity":"` + verifyTestSsecurity + `","passToken":"pt","location":"` + pairedLoc + `"}`))}, nil
		case strings.Contains(req.URL, "sts.api.io.mi.com/sts"):
			return plugin.HTTPSessionResponse{Status: 200, BodyB64: base64.StdEncoding.EncodeToString([]byte("ok"))}, nil
		case strings.Contains(req.URL, "device_list"):
			body, _ := base64.StdEncoding.DecodeString(req.BodyB64)
			q, _ := url.ParseQuery(string(body))
			signed, err := signedNonceKey2(verifyTestSsecurity, q.Get("_nonce"))
			if err != nil {
				t.Fatal(err)
			}
			key, _ := base64.StdEncoding.DecodeString(signed)
			plain := []byte(`{"code":0,"result":{"list":[]}}`)
			return plugin.HTTPSessionResponse{Status: 200, BodyB64: base64.StdEncoding.EncodeToString([]byte(base64.StdEncoding.EncodeToString(rc4Crypt(key, plain))))}, nil
		}
		t.Fatalf("意外请求 %s", req.URL)
		return nil, nil
	})

	out, err := call(&plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"},
		"verify", json.RawMessage(`{"session_id":"s1","code":"246810"}`))
	if err != nil {
		t.Fatalf("配对刷新路径失败: %v", err)
	}
	if !strings.Contains(string(out), "completed") {
		t.Fatalf("应 completed：%s", out)
	}
	// 断言顺序：verify loc → serviceLogin → paired loc → device_list
	idx := func(sub string) int {
		for i, u := range order {
			if strings.Contains(u, sub) {
				return i
			}
		}
		return -1
	}
	iVerify, iLogin, iPaired, iProbe := idx("d=verify"), idx("serviceLogin"), idx("d=paired"), idx("device_list")
	if iVerify < 0 || iLogin < 0 || iPaired < 0 || iProbe < 0 {
		t.Fatalf("缺少关键调用: %v", order)
	}
	if !(iVerify < iLogin && iLogin < iPaired && iPaired < iProbe) {
		t.Fatalf("顺序错误（必须 verify→step1→配对location→探针）: %v", order)
	}
}
