package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
	"testing"
)

func TestLegacyMigrationFixtureAtomicOrder(t *testing.T) {
	h := hosttest.New(t)
	session := []byte(`{"version":1,"region_id":"cn","masked_user":"u***","service_token":"sealed-legacy","ssecurity":"legacy-secret","user_id":"42"}`)
	sum := sha256.Sum256(session)
	h.Fixture(plugin.OpLegacyStatus, plugin.LegacyStatusResponse{Source: "hx_mi_legacy"}).Fixture(plugin.OpLegacySnapshot, plugin.LegacySnapshotResponse{SnapshotID: "snap", Source: "hx_mi_legacy", Files: 1, Bytes: int64(len(session))}).Fixture(plugin.OpLegacyList, map[string]any{"files": []plugin.LegacyFileInfo{{Path: "session.enc", Size: int64(len(session)), SHA256: base64.StdEncoding.EncodeToString(sum[:])}}}).Fixture(plugin.OpLegacyRead, map[string]any{"data_b64": base64.StdEncoding.EncodeToString(session)}).Fixture(plugin.OpFSWrite, map[string]any{"ok": true}).Fixture(plugin.OpFSRead, map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(session)}).Fixture(plugin.OpLegacyCommit, map[string]any{"ok": true})
	if err := migrateLegacy(&plugin.Context{}); err != nil {
		t.Fatal(err)
	}
	calls := h.Calls()
	if calls[len(calls)-1].Op != plugin.OpLegacyCommit {
		t.Fatalf("last=%s", calls[len(calls)-1].Op)
	}
}
func TestProtocolFixtureMatrix(t *testing.T) {
	fixtures := []struct {
		raw   string
		state string
	}{{`&&&START&&&{"_sign":"s","qs":"q","callback":"c"}`, "step1"}, {`{"captchaUrl":"/pass/getCode"}`, "captcha"}, {`{"notificationUrl":"https://account.xiaomi.com/identity/auth/verify"}`, "verify"}, {`{"location":"https://sts.api.io.mi.com/sts","ssecurity":"c2Vj"}`, "sts"}, {`{"code":70016,"desc":"bad"}`, "error"}}
	for _, f := range fixtures {
		v, e := parseAuth([]byte(f.raw))
		if e != nil {
			t.Fatal(e)
		}
		switch f.state {
		case "step1":
			if v.Sign == "" {
				t.Fatal(f.raw)
			}
		case "captcha":
			if v.CaptchaURL == "" {
				t.Fatal(f.raw)
			}
		case "verify":
			if v.NotificationURL == "" {
				t.Fatal(f.raw)
			}
		case "sts":
			if v.Location == "" || v.Ssecurity == "" {
				t.Fatal(f.raw)
			}
		case "error":
			if explainLoginCode(v.Code, v.Desc) == "" {
				t.Fatal(f.raw)
			}
		}
	}
}
func TestV2MetadataFixtureNoSecrets(t *testing.T) {
	b, _ := json.Marshal(savedCloudSession{Version: 2, SecretRef: "xiaomi_session_material", SecretVersion: 9, TicketsSealed: true})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"ssecurity", "pass_token", "service_token", "user_id"} {
		if _, ok := m[k]; ok {
			t.Fatal(k)
		}
	}
}
