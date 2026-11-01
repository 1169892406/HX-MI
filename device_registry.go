package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"homex.dev/homex/sdk/go/plugin"
	"strings"
)

func loadDeviceRegistry(ctx *plugin.Context) ([]miioDevice, error) {
	r, err := ctx.FSRead("devices.json")
	if err != nil || !r.Found {
		return nil, errors.New("设备登记不存在")
	}
	b, err := r.Data()
	if err != nil {
		return nil, err
	}
	var v2 deviceRegistryV2
	// ## 为什么不能要求 v2.Devices != nil（"设备登记损坏"的真凶）
	//
	// 空列表在 Go 里 `json.Marshal(nil slice)` 得到 `"devices":null`，
	// 而不是 `[]`。于是"云端一台设备都没有"这种**完全正常**的状态会被
	// 这条判据当成损坏文件：用户点手动同步 → 报「设备登记损坏」。
	// 一个空账号不该等于一个坏文件。版本号合法就认，nil 当空表处理。
	if json.Unmarshal(b, &v2) == nil && (v2.Version == 2 || v2.Version == 3) {
		if v2.Version == 3 {
			// 摘要按**写入时的原始字节**校验：空列表在磁盘上可能是 `null`
			// （历史文件）也可能是 `[]`（新写入）。若先把 nil 规范成 [] 再算
			// 摘要，老文件的 sha256（对应 null）永远对不上，会被误报「摘要损坏」。
			// 两种形态都认，只要其中一种与摘要相符即可。
			ok := registryDigestMatches(v2.Devices, v2.SHA256)
			if !ok {
				return nil, errors.New("设备登记摘要损坏")
			}
		}
		if v2.Devices == nil {
			v2.Devices = []miioDevice{}
		}
		return v2.Devices, nil
	}
	var legacy []miioDevice
	if json.Unmarshal(b, &legacy) != nil {
		return nil, errors.New("设备登记损坏")
	}
	normalized, _ := json.MarshalIndent(deviceRegistryV2{Version: 2, Devices: legacy}, "", "  ")
	w := plugin.FSWriteRequest{Path: "devices.json", QuotaKB: 4096}
	w.SetData(normalized)
	if err = ctx.FSWrite(w); err != nil {
		return nil, err
	}
	return legacy, nil
}

// registryDigestMatches 判定 devices 的摘要是否与给定 sha256 相符。
//
// 空列表有两种合法序列化形态（nil → "null"，空切片 → "[]"），历史文件两种都
// 可能存在过。摘要必须对两者都成立，否则"云端没有设备"会被误判成文件损坏
// （用户实测：手动同步报「设备登记损坏」）。
func registryDigestMatches(devices []miioDevice, want string) bool {
	try := func(v any) bool {
		payload, err := json.Marshal(v)
		if err != nil {
			return false
		}
		sum := sha256.Sum256(payload)
		return strings.EqualFold(want, fmt.Sprintf("%x", sum[:]))
	}
	if try(devices) {
		return true
	}
	if devices == nil {
		return try([]miioDevice{})
	}
	return false
}
