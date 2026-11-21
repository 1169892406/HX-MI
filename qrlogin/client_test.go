package qrlogin

// 扫码登录端到端测试（不联网，httptest 模拟 passport）。
// 覆盖：bootstrap → 长轮询成功 → clientSign 回调 → token 组装落盘（.mi.token
// 与 auth.json）→ 免密续期时 passToken 轮换被收下。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mockPassport：
//   - …/serviceLogin：按 svc 脚本返回（&&&START&&& 前缀）
//   - 其余路径：视为「location 回调」，把 cookies 作为 Set-Cookie 下发
type mockPassport struct {
	svc     map[string]any
	cookies map[string]string
}

func (m *mockPassport) handler(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/serviceLogin") {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "&&&START&&&"+mustJSON(m.svc))
		return
	}
	if strings.HasSuffix(r.URL.Path, "/longPolling/loginUrl") {
		fmt.Fprint(w, "&&&START&&&"+mustJSON(map[string]any{
			"loginUrl": "https://mock.local/qr",
			"lp":       m.svc["lp"],
			"timeout":  240,
		}))
		return
	}
	if strings.HasSuffix(r.URL.Path, "/lp") {
		fmt.Fprint(w, "&&&START&&&"+mustJSON(m.svc["lpResult"]))
		return
	}
	if r.URL.Query().Get("clientSign") == "" {
		w.WriteHeader(400)
		fmt.Fprint(w, "missing clientSign")
		return
	}
	for k, v := range m.cookies {
		http.SetCookie(w, &http.Cookie{Name: k, Value: v})
	}
	fmt.Fprint(w, "ok")
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func newTestClient(t *testing.T, svc map[string]any, cookies map[string]string) (*Client, string) {
	t.Helper()
	mp := &mockPassport{svc: svc, cookies: cookies}
	srv := httptest.NewServer(http.HandlerFunc(mp.handler))
	t.Cleanup(srv.Close)

	for k, v := range svc {
		if s, ok := v.(string); ok {
			svc[k] = strings.ReplaceAll(s, "{{LOC}}", srv.URL)
		}
	}

	dir := t.TempDir()
	tokPath := filepath.Join(dir, ".mi.token")
	c := New(Options{
		TokenPath: tokPath,
		Passport:  srv.URL,
		HTTP:      srv.Client(),
		ServiceHTTP: srv.Client(),
	})
	return c, tokPath
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		t.Fatalf("解析 %s 失败: %s", path, data)
	}
	return m
}

// 端到端：扫码 → 轮询到 ok → token 落盘（.mi.token + auth.json）。
func TestScanAndPersist(t *testing.T) {
	lpDone := make(chan struct{})
	svc := map[string]any{
		"location": "{{LOC}}", "nonce": "n0", "ssecurity": "ss_scan", // bootstrap 的 serviceLogin 响应
		"lp": "{{LOC}}/lp",
		"lpResult": map[string]any{
			"code": 0, "location": "{{LOC}}", "nonce": "n1", "ssecurity": "ss_new",
			"userId": 319, "passToken": "PT_SCAN",
		},
	}
	// 长轮询第一次就返回成功
	cookies := map[string]string{"serviceToken": "ST_SCAN", "passToken": "PT_CB", "cUserId": "CU"}
	_ = lpDone

	c, tokPath := newTestClient(t, svc, cookies)
	st, err := c.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.State != "waiting" {
		t.Fatalf("Start 状态 = %s（期望 waiting）", st.State)
	}
	if st.LoginURL == "" {
		t.Fatal("Start 未返回二维码链接")
	}

	// 轮询到 ok（最长 5 秒）
	deadline := time.Now().Add(5 * time.Second)
	var final Status
	for time.Now().Before(deadline) {
		final = c.Status()
		if final.State == "ok" || final.State == "error" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if final.State != "ok" {
		t.Fatalf("扫码未成功: state=%s msg=%s", final.State, final.Message)
	}
	if final.UserID != "319" {
		t.Errorf("UserID = %q（期望 319）", final.UserID)
	}

	// token 落盘断言
	disk := readJSON(t, tokPath)
	if disk["passToken"] != "PT_SCAN" && disk["passToken"] != "PT_CB" {
		t.Errorf(".mi.token passToken = %v（期望 PT_SCAN/PT_CB）", disk["passToken"])
	}
	tokAny, ok := disk[sid].([]any)
	if !ok || len(tokAny) != 2 || tokAny[1] != "ST_SCAN" {
		t.Errorf(".mi.token micoapi = %v（期望 [ss_new ST_SCAN]）", disk[sid])
	}
	root := readJSON(t, filepath.Join(filepath.Dir(tokPath), "auth.json"))
	if root["passToken"] == "" {
		t.Error("auth.json 未落盘根凭据")
	}
	if !c.HasToken() {
		t.Error("HasToken = false")
	}
	if c.tokenSnapshot().ServiceToken() != "ST_SCAN" {
		t.Errorf("ServiceToken = %q", c.tokenSnapshot().ServiceToken())
	}
}

// 免密续期：passToken 有效 → serviceLogin code=0 → 回调轮换 passToken 必须被收下。
func TestRefreshRotatesPassToken(t *testing.T) {
	token := Token{"deviceId": "wb_test", "userId": "319", "passToken": "PT_OLD",
		sid: []string{"ss_old", "ST_OLD"}}
	svc := map[string]any{
		"lp": "{{LOC}}/lp",
		"code": 0, "location": "{{LOC}}", "nonce": "n1", "ssecurity": "ss_new",
		"userId": 319,
	}
	cookies := map[string]string{"serviceToken": "ST_NEW", "passToken": "PT_ROTATED", "cUserId": "CU"}

	c, tokPath := newTestClient(t, svc, cookies)
	if err := os.WriteFile(tokPath, []byte(mustJSON(token)), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Load()

	uid, err := c.Refresh()
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if uid != "319" {
		t.Errorf("Refresh uid = %q", uid)
	}
	tok := c.tokenSnapshot()
	if tok.str("passToken") != "PT_ROTATED" {
		t.Errorf("轮换后的 passToken 未被收下 = %q（期望 PT_ROTATED）", tok.str("passToken"))
	}
	if tok.ServiceToken() != "ST_NEW" {
		t.Errorf("续期后的 serviceToken = %q（期望 ST_NEW）", tok.ServiceToken())
	}
	disk := readJSON(t, tokPath)
	if disk["passToken"] != "PT_ROTATED" {
		t.Errorf("磁盘 passToken 未更新 = %v", disk["passToken"])
	}
}

// 70016：passToken 失效 → need_password，且不得破坏内存/磁盘里的现有凭据。
func TestRefreshNeedPassword(t *testing.T) {
	token := Token{"deviceId": "wb_test", "userId": "319", "passToken": "PT_NEW",
		sid: []string{"ss_new", "ST_NEW"}}
	svc := map[string]any{
		"lp":    "{{LOC}}/lp",
		"code":  70016, "description": "登录验证失败",
	}
	c, tokPath := newTestClient(t, svc, nil)
	if err := os.WriteFile(tokPath, []byte(mustJSON(token)), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Load()

	if _, err := c.Refresh(); err == nil {
		t.Fatal("Refresh 应失败")
	}
	tok := c.tokenSnapshot()
	if tok.str("passToken") != "PT_NEW" {
		t.Errorf("失败后内存 passToken = %q（期望 PT_NEW）", tok.str("passToken"))
	}
	disk := readJSON(t, tokPath)
	if disk["passToken"] != "PT_NEW" {
		t.Errorf("失败后磁盘 passToken = %v（期望 PT_NEW）", disk["passToken"])
	}
}
