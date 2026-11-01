package main

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"homex.dev/homex/sdk/go/plugin"
)

type orderedKV struct{ K, V string }

func genNonceAt(now time.Time) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b[:8]); err != nil {
		return "", err
	}
	binary.BigEndian.PutUint32(b[8:], uint32(now.UnixMilli()/60000))
	return base64.StdEncoding.EncodeToString(b), nil
}
func signedNonceKey2(ssecurity, nonce string) (string, error) {
	ss, e := base64.StdEncoding.DecodeString(ssecurity)
	if e != nil {
		return "", e
	}
	nn, e := base64.StdEncoding.DecodeString(nonce)
	if e != nil {
		return "", e
	}
	sum := sha256.Sum256(append(append([]byte{}, ss...), nn...))
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}
func rc4Value(signed, plain string) (string, error) {
	key, e := base64.StdEncoding.DecodeString(signed)
	if e != nil {
		return "", e
	}
	return base64.StdEncoding.EncodeToString(rc4Crypt(key, []byte(plain))), nil
}
func rc4Sign(method, rawURL string, params []orderedKV, signed string) string {
	u, _ := url.Parse(rawURL)
	p := u.Path
	if strings.HasPrefix(p, "/app/") {
		p = p[4:]
	}
	parts := []string{strings.ToUpper(method), p}
	for _, v := range params {
		parts = append(parts, v.K+"="+v.V)
	}
	parts = append(parts, signed)
	sum := sha1.Sum([]byte(strings.Join(parts, "&")))
	return base64.StdEncoding.EncodeToString(sum[:])
}
func buildRC4Body(apiURL, ssecurity, data, nonce string) (url.Values, string, error) {
	signed, e := signedNonceKey2(ssecurity, nonce)
	if e != nil {
		return nil, "", e
	}
	plain := []orderedKV{{"data", data}}
	plain = append(plain, orderedKV{"rc4_hash__", rc4Sign("POST", apiURL, plain, signed)})
	enc := make([]orderedKV, 0, len(plain))
	for _, p := range plain {
		v, e := rc4Value(signed, p.V)
		if e != nil {
			return nil, "", e
		}
		enc = append(enc, orderedKV{p.K, v})
	}
	form := url.Values{}
	for _, p := range enc {
		form.Set(p.K, p.V)
	}
	form.Set("signature", rc4Sign("POST", apiURL, enc, signed))
	form.Set("ssecurity", ssecurity)
	form.Set("_nonce", nonce)
	return form, signed, nil
}
func apiPostRC4Guest(ctx *plugin.Context, sessionID, ssecurity, apiURL, data string) ([]byte, error) {
	return apiPostRC4GuestDevice(ctx, sessionID, ssecurity, "", apiURL, data)
}

// apiPostRC4GuestDevice 额外携带小米设备标识。
//
// 旧版 protocolClient 对每个请求（含 device_list）都带 deviceId/sdkVersion；
// 新版探针之前没带，小米会以 401 拒绝已换到的 serviceToken。deviceID 为空时
// 保持旧行为（历史调用点不受影响）。
func apiPostRC4GuestDevice(ctx *plugin.Context, sessionID, ssecurity, deviceID, apiURL, data string) ([]byte, error) {
	if ssecurity == "" {
		return nil, errors.New("缺少 ssecurity")
	}
	nonce, e := genNonceAt(time.Now())
	if e != nil {
		return nil, e
	}
	form, signed, e := buildRC4Body(apiURL, ssecurity, data, nonce)
	if e != nil {
		return nil, e
	}
	headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept-Encoding": "identity", "MIOT-ENCRYPT-ALGORITHM": "ENCRYPT-RC4", "X-XIAOMI-PROTOCAL-FLAG-CLI": "PROTOCAL-HTTP2", "User-Agent": "Android-7.1.1-1.0.0-ONEPLUS A3010-136-"}
	// 旧版/上游对每个业务请求都显式带一组静态 cookie（locale/channel/timezone/
	// is_daylight/dst_offset）。它们不是服务端下发的，jar 里没有；缺了会被小米
	// 判为 auth error（返回明文错误信封）。yetAnotherServiceToken/serviceToken/
	// userId/cUserId 由宿主票据 attach 注入 jar，Go http.Client 会把 jar cookie
	// 追加到手工 Cookie 头之后，两者并存。
	staticCookies := "locale=zh_CN; channel=MI_APP_STORE; timezone=GMT+08:00; is_daylight=0; dst_offset=0"
	if deviceID != "" {
		headers["User-Agent"] = fmt.Sprintf(xiaomiUATemplate, deviceID)
		headers["Cookie"] = "deviceId=" + deviceID + "; sdkVersion=" + xiaomiSDKVersion + "; " + staticCookies
	} else {
		headers["Cookie"] = staticCookies
	}
	req := plugin.HTTPSessionRequest{SessionID: sessionID, Method: "POST", URL: apiURL, Headers: headers, TimeoutMS: 20000}
	req.SetBody([]byte(form.Encode()))
	resp, e := ctx.HTTPSessionDo(req)
	if e != nil {
		return nil, fmt.Errorf("网络不确定: %w", e)
	}
	body, e := resp.Body()
	if e != nil {
		return nil, e
	}
	if resp.Status == 401 || resp.Status == 403 {
		return nil, fmt.Errorf("认证票据被明确拒绝（host=%s，status=%d）", hostOf(apiURL), resp.Status)
	}
	if resp.Status >= 500 {
		return nil, fmt.Errorf("网络不确定: HTTP %d", resp.Status)
	}
	// 小米的成功响应是 **base64 文本**形式的 RC4 密文（旧版 decryptRC4Value
	// 先 base64 解码再 RC4）；此前直接把 wire 字节当密文是错的，会在认证
	// 修好后立刻变成"响应不确定"。明文 JSON 则是业务错误信封——必须把
	// code/message 透出来：盲报"错误信封"让排查绕了远路（信封里的原文
	// 才是小米的真实抱怨，例如 auth error / 签名不符 / 频控）。
	text := strings.TrimSpace(string(body))
	if strings.HasPrefix(text, "{") {
		var er struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
			Desc    string `json:"desc"`
		}
		if json.Unmarshal([]byte(text), &er) == nil && (er.Message != "" || er.Desc != "" || er.Code != nil) {
			detail := firstNonEmpty(er.Message, er.Desc)
			if len(detail) > 160 {
				detail = detail[:160]
			}
			return nil, fmt.Errorf("小米业务接口拒绝（code=%v，说明：%s）", er.Code, detail)
		}
		return nil, errors.New("业务接口返回未加密 JSON，拒绝作为成功响应")
	}
	return decryptRC4Text(signed, text)
}
func decryptRC4Bytes(signed string, cipherBytes []byte) ([]byte, error) {
	key, e := base64.StdEncoding.DecodeString(signed)
	if e != nil {
		return nil, e
	}
	if len(cipherBytes) == 0 {
		return nil, errors.New("RC4 响应密文为空")
	}
	return rc4Crypt(key, cipherBytes), nil
}

// decryptRC4Text 仅供明确拿到上游 base64 文本的兼容/测试路径。
func decryptRC4Text(signed, cipherText string) ([]byte, error) {
	raw, e := base64.StdEncoding.DecodeString(strings.TrimSpace(cipherText))
	if e != nil {
		return nil, e
	}
	return decryptRC4Bytes(signed, raw)
}
