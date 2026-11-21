package qrlogin

// HTTP 辅助：全部走 Transport 抽象（默认 net/http，wasm 里换成 Host API）。

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

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
