package qrlogin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HTTP 辅助：全部走 Transport 抽象（默认 net/http，wasm 里换成 Host API）。

// parsePassportJSON 解析 passport 响应体，**JSON number 一律保留为 json.Number**。
//
// # 为什么必须 UseNumber（2026-11-22，扫码 invalid signature 的最终根因）
//
// passport 的 `nonce` 是 19 位整数（例：6019617419463721984）。默认的
// json.Unmarshal 会把它解成 float64 —— 而 float64 只有 53 位尾数，**存不下**：
//
//	float64(6019617419463721984) → 6019617419463722000   （差了 16）
//	fmt.Sprint(...)              → "6.019617419463722e+18"
//
// clientSign = base64(sha1("nonce=" + nonce + "&" + ssecurity))：
// nonce 只要差 1 位，签名就完全错，服务端不下发有效 serviceToken，
// 米家 RC4 接口于是回 `{"code":0,"message":"invalid signature"}`。
// `code=0` 让它看起来像成功，文案又把人引向"签名算法错了"，极难自查。
//
// 用 UseNumber 后 nonce 解成字符串原样保留，strOf 直接返回，精度零损失。
func parsePassportJSON(body []byte, out any) error {
	raw := passportStrip(body)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(out)
}

// respHeaders 规范化响应头：net/http 版已带；Host API 版把 Set-Cookie 逐行放进
// Headers["Set-Cookie"]（多值用 \n 分隔）——cookie 收集器按行解析。
func parseSetCookies(hdr map[string]string) []cookie {
	out := []cookie{}
	for _, k := range []string{"Set-Cookie", "set-cookie"} {
		raw, ok := hdr[k]
		if !ok {
			continue
		}
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if c, err := parseSetCookieLine(line); err == nil {
				out = append(out, c)
			}
		}
	}
	return out
}

type cookie struct {
	Name  string
	Value string
}

func parseSetCookieLine(line string) (cookie, error) {
	parts := strings.SplitN(line, ";", 2)
	kv := strings.SplitN(strings.TrimSpace(parts[0]), "=", 2)
	if len(kv) != 2 {
		return cookie{}, fmt.Errorf("bad set-cookie line")
	}
	return cookie{Name: strings.TrimSpace(kv[0]), Value: strings.TrimSpace(kv[1])}, nil
}

// trDo 走 Transport 发 GET。
func (c *Client) trDo(u string, headers map[string]string, timeout time.Duration) (Response, error) {
	return c.tr.Do(http.MethodGet, u, headers, timeout)
}

func (c *Client) httpGet(u string, headers map[string]string) ([]byte, int, error) {
	resp, err := c.trDo(u, headers, 30*time.Second)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.Status, nil
}

// httpGetSvc serviceLogin 免密流程用（与长轮询不同 Client 时可用）。
func (c *Client) httpGetSvc(u string, headers map[string]string) ([]byte, int, error) {
	return c.httpGet(u, headers)
}

// httpGetStop 带轮询停止通道：stop 关闭时中断（默认实现靠 timeout 兜底，
// Transport 无法真正取消时由上层在收到结果后丢弃）。
func (c *Client) httpGetStop(u string, headers map[string]string, stop chan struct{}) ([]byte, int, error) {
	// stop 的语义是「扫码被取消」：长轮询请求本身照发（默认实现无法取消），
	// 结果返回后由调用方丢弃。受限运行时里 timeout 会自然断开。
	return c.httpGet(u, headers)
}

// httpCookieJar 收下一次响应的全部 Set-Cookie。
// 关键用途：serviceLogin 回调响应里带轮换后的 passToken/cUserId，
// 只取「Cookie: serviceToken=…」会把新凭据丢掉（miservice 的 70016 死循环根因）。
func (c *Client) httpCookieJar(u string, headers map[string]string, stop chan struct{}) (map[string]string, error) {
	resp, err := c.trDo(u, headers, 30*time.Second)
	if err != nil {
		return nil, err
	}
	jar := map[string]string{}
	for _, c := range parseSetCookies(resp.Headers) {
		if c.Value != "" {
			jar[c.Name] = c.Value
		}
	}
	return jar, nil
}
