package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"homex.dev/homex/sdk/go/plugin"
)

const jsonpPrefix = "&&&START&&&"

type authResponse struct {
	Code            any    `json:"code"`
	Desc            string `json:"desc"`
	Description     string `json:"description"`
	Sign            string `json:"_sign"`
	QS              string `json:"qs"`
	Callback        string `json:"callback"`
	Location        string `json:"location"`
	NotificationURL string `json:"notificationUrl"`
	CaptchaURL      string `json:"captchaUrl"`
	Ssecurity       string `json:"ssecurity"`
	PassToken       string `json:"passToken"`
	// UserID 是小米数字账号 ID（step2 响应里的 userId）。step1 需要与
	// passToken 一起发送，小米才认"同一个已登录账号"并下发配对材料。
	UserID any `json:"userId"`
}

// userIDString 把 userId 统一成字符串（小米有时给数字、有时给字符串）。
func userIDString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	default:
		return fmt.Sprint(t)
	}
}
type loginChallenge struct {
	Kind       string `json:"kind"`
	SendStatus string `json:"send_status"`
	VerifyURL  string `json:"verify_url,omitempty"`
	CaptchaURL string `json:"captcha_url,omitempty"`
	Target     string `json:"target,omitempty"`
}
type protocolState struct {
	Sign       string          `json:"sign"`
	QS         string          `json:"qs"`
	Callback   string          `json:"callback"`
	CaptchaURL string          `json:"captcha_url,omitempty"`
	Challenge  *loginChallenge `json:"challenge,omitempty"`
}

func stripJSONP(b []byte) []byte {
	s := strings.TrimSpace(string(b))
	s = strings.TrimPrefix(s, jsonpPrefix)
	// 小米部分端点返回 &&&START&&&{...}&&&END&&&；只去前缀会让尾部符号
	// 把 JSON 解析打断，表现为“响应不是合法 JSON”（实测验证码链路踩到）。
	s = strings.TrimSuffix(strings.TrimSpace(s), "&&&END&&&")
	return []byte(strings.TrimSpace(s))
}
func parseAuth(b []byte) (authResponse, error) {
	var v authResponse
	if err := json.Unmarshal(stripJSONP(b), &v); err != nil {
		return v, errors.New("解析小米登录响应失败，可能是接口变更")
	}
	return v, nil
}
func md5Password(p string) string {
	s := md5.Sum([]byte(p))
	return strings.ToUpper(hex.EncodeToString(s[:]))
}
func formBody(user, password string, p protocolState, captcha string) []byte {
	v := url.Values{}
	v.Set("user", user)
	v.Set("hash", md5Password(password))
	v.Set("callback", p.Callback)
	v.Set("sid", "xiaomiio")
	v.Set("qs", p.QS)
	v.Set("_sign", p.Sign)
	if captcha != "" {
		v.Set("captCode", captcha)
	}
	return []byte(v.Encode())
}
func doStep2(ctx *plugin.Context, sessionID, user, password, captcha string, p protocolState, deviceID string) (authResponse, error) {
	u := "https://account.xiaomi.com/pass/serviceLoginAuth2?_json=true"
	if captcha != "" {
		u += fmt.Sprintf("&_dc=%d", time.Now().UnixMilli())
	}
	req := plugin.HTTPSessionRequest{SessionID: sessionID, Method: "POST", URL: u, Headers: mergeHeaders(map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, xiaomiHeaders(deviceID)), TimeoutMS: 20000}
	req.SetBody(formBody(user, password, p, captcha))
	resp, err := ctx.HTTPSessionDo(req)
	if err != nil {
		return authResponse{}, fmt.Errorf("网络不确定: %w", err)
	}
	body, err := resp.Body()
	if err != nil {
		return authResponse{}, err
	}
	if resp.Status == 401 || resp.Status == 403 {
		return authResponse{Code: 70016, Desc: "认证被明确拒绝"}, nil
	}
	if resp.Status != 200 {
		return authResponse{}, fmt.Errorf("网络不确定: 小米账号服务 HTTP %d", resp.Status)
	}
	return parseAuth(body)
}
func explainLoginCode(code any, desc string) string {
	c := fmt.Sprint(code)
	switch c {
	case "70016", "87001":
		return "账号、密码或验证码不正确"
	case "70002":
		return "账号不存在"
	case "70008":
		return "登录尝试过多，请稍后再试"
	case "0", "<nil>":
		return desc
	default:
		if desc != "" {
			return desc
		}
		return "小米账号服务拒绝登录（代码 " + c + "）"
	}
}
