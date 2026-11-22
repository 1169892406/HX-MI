package qrlogin

// Token 与落盘：.mi.token（miservice 格式）+ auth.json（根凭据，只增不删）。

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Token 落盘凭据。sid 键（micoapi）的值是 [ssecurity, serviceToken]。
type Token map[string]any

func (t Token) str(key string) string {
	if t == nil {
		return ""
	}
	if v, ok := t[key]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

// ServiceToken micoapi 的 serviceToken（mina/对话接口的鉴权凭据）。
func (t Token) ServiceToken() string {
	if v, ok := t[sid].([]any); ok && len(v) >= 2 {
		return fmt.Sprint(v[1])
	}
	if v, ok := t[sid].([]string); ok && len(v) >= 2 {
		return v[1]
	}
	return ""
}

// UserID 用户 id。
func (t Token) UserID() string { return t.str("userId") }

// PassToken 长期凭据（免密续期用）。
func (t Token) PassToken() string { return t.str("passToken") }

// ---------- Client 侧的 token 存取 ----------

func (c *Client) tokenSnapshot() Token {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.inner
	if t == nil {
		t = Token{}
	}
	return t
}

func (c *Client) setToken(tok Token) {
	c.mu.Lock()
	c.inner = tok
	c.mu.Unlock()
}

// Load 从磁盘恢复 token（New 时自动调用；也可在落盘被外部更新后手动调用）。
func (c *Client) Load() bool {
	t := c.loadToken()
	c.mu.Lock()
	c.inner = t
	c.mu.Unlock()
	return t.HasToken()
}

// HasToken 是否已有可用的 serviceToken。
func (c *Client) HasToken() bool {
	return c.tokenSnapshot().HasToken()
}

// HasToken 有 serviceToken 才算可用。
func (t Token) HasToken() bool { return t.ServiceToken() != "" }

func (c *Client) loadToken() Token {
	tok := Token(c.readTokenKey(c.o.TokenPath))
	if !tok.HasToken() {
		// 扁平 token（serviceToken/ssecurity 在顶层）就地补成 miservice 结构
		st, ss := tok.str("serviceToken"), tok.str("ssecurity")
		if st != "" && ss != "" {
			tok[sid] = []string{ss, st}
			c.writeTokenKey(c.o.TokenPath, tok)
		}
	}
	// auth.json 的长期凭据兜底（只增不删）
	if root := Token(c.readTokenKey(c.rootKey())); root != nil {
		for _, k := range []string{"passToken", "userId", "deviceId", "cUserId"} {
			if v := root.str(k); v != "" && tok.str(k) == "" {
				tok[k] = v
			}
		}
	}
	return tok
}

// rootKey 根凭据键（与 .mi.token 同目录同名文件；Store 实现里目录由键名自由表达）。
func (c *Client) rootKey() string {
	dir, _ := splitKey(c.o.TokenPath)
	return joinKey(dir, "auth.json")
}

// persist 原子写 .mi.token + auth.json（根凭据只增不删）。
// auth.json 与 .mi.token 分开存的原因：续期代码只会动前者，根凭据被误删/写坏时
// 还能用后者兜底（对照 xiaomusic 的做法）。
func (c *Client) persist(tok Token) {
	c.writeTokenKey(c.o.TokenPath, tok)
	root := Token{}
	for _, k := range []string{"passToken", "userId", "deviceId", "cUserId", "saveTime"} {
		if v := tok.str(k); v != "" {
			root[k] = v
		}
	}
	if root.str("passToken") == "" {
		return
	}
	path := c.rootKey()
	if old := c.readTokenKey(path); old != nil {
		for k, v := range old {
			if _, ok := root[k]; !ok {
				root[k] = v
			}
		}
	}
	c.writeTokenKey(path, root)
}

// readTokenKey 经 Store 读一个 JSON 键（文件/存储由实现决定）。
func (c *Client) readTokenKey(key string) map[string]any {
	raw, found, err := c.store.get(key)
	if err != nil || !found {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return nil
	}
	return m
}

// writeTokenKey 经 Store 原子写一个 JSON 键。
func (c *Client) writeTokenKey(key string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	if err := c.store.put(key, string(data)); err != nil {
		c.logf("落盘 %s 失败: %v", key, err)
	}
}

// ---------- 通用工具 ----------

func parseMiJSON(body []byte, status int) (map[string]any, error) {
	if status != 200 {
		return nil, fmt.Errorf("HTTP %d: %.120s", status, string(body))
	}
	raw := strings.TrimPrefix(strings.TrimSpace(string(body)), "&&&START&&&")
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("响应不是 JSON: %.120s", raw)
	}
	return out, nil
}

func passportStrip(b []byte) []byte {
	return []byte(strings.TrimSpace(strings.ReplaceAll(string(b), "&&&START&&&", "")))
}

func strOf(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func intOf(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		var n int
		fmt.Sscanf(x, "%d", &n)
		return n
	}
	return 0
}

func floatOf(v any, def float64) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		var f float64
		fmt.Sscanf(x, "%f", &f)
		return f
	}
	return def
}

// splitKey/joinKey 键名目录拆分（FileStore 下就是 filepath 语义；
// 其他 Store（如插件 StorageGet/Put）可把键名原样当 key）。
func splitKey(key string) (string, string) {
	if i := strings.LastIndex(key, "/"); i >= 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}

func joinKey(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// tokenLikeBody 判断回调响应体是否像"裸 token"。
//
// 兜底分支曾把 <512 字节的响应体一律当 serviceToken 收下——小米回错误页/
// 错误 JSON 时会"假成功"并把垃圾落盘成凭据。这里只收明确不像页面/JSON 的内容。
func tokenLikeBody(b []byte) bool {
	if len(b) == 0 || len(b) >= 512 {
		return false
	}
	s := string(b)
	if strings.ContainsAny(s, "<>{}") || strings.Contains(strings.ToLower(s), "html") {
		return false
	}
	// token 是连续可打印字符（无空白/换行）
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return false
		}
	}
	return true
}

func randomHex(n int) string {
	const hexdigits = "0123456789ABCDEF"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = hexdigits[int(b[i])%16]
	}
	return string(b)
}

// RandomUA 导出的随机机型 UA（宿主适配层组装请求头用；与 randomUA 同一生成器）。
func RandomUA() string { return randomUA() }

func randomUA() string {
	h := func(n int) string {
		const hexdigits = "0123456789ABCDEF"
		b := make([]byte, n)
		_, _ = rand.Read(b)
		for i := range b {
			b[i] = hexdigits[int(b[i])%16]
		}
		return string(b)
	}
	part := func(n int) string {
		const chars = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_-"
		b := make([]byte, n)
		_, _ = rand.Read(b)
		for i := range b {
			b[i] = chars[int(b[i])%len(chars)]
		}
		return string(b)
	}
	return "Android-15-11.0.701-Xiaomi-23046RP50C-OS2.0.212.0.VMYCNXM-" +
		h(40) + "-CN-" + h(32) + "-" + h(32) + "-SmartHome-MI_APP_STORE-" +
		h(40) + "|" + h(40) + "|" + part(16) + "-64"
}

func sha1Sum(b []byte) []byte {
	h := sha1.New()
	h.Write(b)
	return h.Sum(nil)
}

func base64Std(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

var _ = time.Now // 保持 time 引用
