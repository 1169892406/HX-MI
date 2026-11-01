package main

import (
	"encoding/base64"
	"encoding/json"
	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
	"net/url"
	"strings"
	"testing"
)

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
func TestRestoreHarnessOrderAndSameSession(t *testing.T) {
	h := hosttest.New(t)
	fs := savedCloudSession{Version: 2, SecretRef: "xiaomi_session_material", SecretVersion: 1, TicketsSealed: true}
	fb, _ := json.Marshal(fs)
	matSsecurity := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	mat, _ := json.Marshal(cloudSecretMaterial{Ssecurity: matSsecurity})
	h.Fixture(plugin.OpFSRead, map[string]any{"found": true, "data_b64": b64(fb)}).OnHTTPOpen(plugin.HTTPOpenResponse{SessionID: "new-s"}).OnHTTPCookiesAttach(plugin.HTTPCookiesAttachResponse{Attached: []plugin.CookieTicketInfo{{Name: "serviceToken", Presence: true}}}).OnVaultBlobOpen(plugin.VaultBlobOpenResponse{Key: "xiaomi_session_material", Version: 1, DataB64: b64(mat)})
	h.On(plugin.OpHTTPSessionRequest, func(req json.RawMessage) (any, error) {
		var r plugin.HTTPSessionRequest
		_ = json.Unmarshal(req, &r)
		if r.SessionID != "new-s" {
			t.Fatalf("session=%s", r.SessionID)
		}
		// SDK wire 的 body_b64 只负责跨 ABI；内容必须是真实 RC4 密文字节。
		body, _ := base64.StdEncoding.DecodeString(r.BodyB64)
		vals, _ := url.ParseQuery(string(body))
		signed, _ := signedNonceKey2(matSsecurity, vals.Get("_nonce"))
		key, _ := base64.StdEncoding.DecodeString(signed)
		cipher := rc4Crypt(key, []byte(`{"result":{"list":[]}}`))
		return plugin.HTTPSessionResponse{Status: 200, BodyB64: b64([]byte(b64(cipher)))}, nil
	})
	state, err := restoreCloudSession(&plugin.Context{})
	if err != nil || state != "available" {
		t.Fatalf("%s %v", state, err)
	}
	calls := h.Calls()
	want := []string{plugin.OpFSRead, plugin.OpVaultBlobOpen, plugin.OpHTTPOpen, plugin.OpHTTPCookiesAttach, plugin.OpHTTPCookiesAttach, plugin.OpHTTPCookiesAttach, plugin.OpHTTPSessionRequest}
	for i, w := range want {
		if i >= len(calls) || calls[i].Op != w {
			t.Fatalf("calls=%v want[%d]=%s", calls, i, w)
		}
	}
}
func TestRestoreHarnessNoTickets(t *testing.T) {
	h := hosttest.New(t)
	fs, _ := json.Marshal(savedCloudSession{Version: 2, SecretRef: "xiaomi_session_material", TicketsSealed: true})
	mat, _ := json.Marshal(cloudSecretMaterial{Ssecurity: "x"})
	h.Fixture(plugin.OpFSRead, map[string]any{"found": true, "data_b64": b64(fs)}).OnVaultBlobOpen(plugin.VaultBlobOpenResponse{DataB64: b64(mat)}).OnHTTPOpen(plugin.HTTPOpenResponse{SessionID: "s"}).OnHTTPCookiesAttach(plugin.HTTPCookiesAttachResponse{})
	state, _ := restoreCloudSession(&plugin.Context{})
	if state != "auth_rejected" {
		t.Fatal(state)
	}
}
func TestMetadataNoSecretMaterial(t *testing.T) {
	b, _ := json.Marshal(savedCloudSession{Version: 2, SecretRef: "xiaomi_session_material", TicketsSealed: true})
	s := string(b)
	for _, x := range []string{"ssecurity", "pass_token", "serviceToken"} {
		if strings.Contains(s, x) {
			t.Fatal(s)
		}
	}
}
