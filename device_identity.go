package main

import (
	"crypto/rand"
	"fmt"
	"net/url"
	"strings"

	"homex.dev/homex/sdk/go/plugin"
)

// 小米账号服务要求客户端带一个稳定的设备标识：
//
//	deviceId cookie = 16位小写字母
//	User-Agent 里内嵌同一个标识
//	sdkVersion=3.8.6
//
// 旧版协议客户端（cloud/client.go）每个请求都带这三样；新版登录早期没带，
// 密码步仍能过，但二次验证后 serviceLogin 不再返回 ssecurity —— 表面是
// “验证已通过但缺 ssecurity”，根因是设备标识缺失。UA 与 deviceId 必须完全
// 一致，否则小米回 70016（看似密码错）。
const (
	xiaomiSDKVersion = "3.8.6"
	xiaomiUATemplate = "Android-7.1.1-1.0.0-ONEPLUS A3010-136-%s APP/xiaomi.smarthome APPV/62830"
)

func randomDeviceID() string {
	b := make([]byte, 16)
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	if _, err := rand.Read(b); err != nil {
		return "homexmiplugin0000"
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// loadOrCreateDeviceID 持久化设备标识。
//
// 旧版把 device_id 落盘（生产为 data/device_id）并在重启后复用，所以小米
// 把它当"同一台已信任设备"，正常登录不需要验证码。新版每次登录随机生成，
// 等于每次换新设备 —— 小米必然触发短信/邮件验证，用户永远走不完。
func loadOrCreateDeviceID(ctx *plugin.Context) (string, error) {
	if r, err := ctx.FSRead("device_id"); err == nil && r.Found {
		if b, derr := r.Data(); derr == nil {
			id := strings.TrimSpace(string(b))
			if validDeviceID(id) {
				return id, nil
			}
		}
	}
	id := randomDeviceID()
	w := plugin.FSWriteRequest{Path: "device_id", QuotaKB: 4}
	w.SetData([]byte(id))
	if err := ctx.FSWrite(w); err != nil {
		return id, err
	}
	return id, nil
}

func validDeviceID(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, c := range s {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

// xiaomiHeaders 返回小米账号接口所需的设备标识头。
// 空 deviceID 时返回 nil（调用方保持旧行为，不让历史会话突然带错标识）。
func xiaomiHeaders(deviceID string) map[string]string {
	if deviceID == "" {
		return nil
	}
	return map[string]string{
		"User-Agent": fmt.Sprintf(xiaomiUATemplate, deviceID),
		"Cookie":     "deviceId=" + deviceID + "; sdkVersion=" + xiaomiSDKVersion,
	}
}

func mergeHeaders(base map[string]string, extra map[string]string) map[string]string {
	if len(extra) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// hostOf 只返回 URL 的主机名（日志/错误用，绝不带 query，避免票据入日志）。
func hostOf(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "-"
	}
	if u, err := url.Parse(s); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return "-"
}
