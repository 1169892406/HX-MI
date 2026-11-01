package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"homex.dev/homex/sdk/go/plugin"
)

type legacyMIIODevice struct {
	ExternalID   string   `json:"external_id"`
	Host         string   `json:"host"`
	TokenHex     string   `json:"token_hex"`
	Model        string   `json:"model"`
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
}
type deviceRegistryV2 struct {
	Version int          `json:"version"`
	Devices []miioDevice `json:"devices"`
	SHA256  string       `json:"sha256,omitempty"`
}

// migrateDeviceTokens 先建立 Driver DeviceContext，再逐设备 seal+open 校验，最后原子覆写
// registry v2。任何设备失败都不删除源、不写最终 registry；已 seal 的 rotatable blob
// 下一轮会安全覆盖，因而可幂等重试。
func migrateDeviceTokens(ctx *plugin.Context, legacy []byte) error {
	var old []legacyMIIODevice
	if err := json.Unmarshal(legacy, &old); err != nil {
		return fmt.Errorf("旧设备登记解析失败: %w", err)
	}
	next := deviceRegistryV2{Version: 2, Devices: make([]miioDevice, 0, len(old))}
	for _, d := range old {
		if d.ExternalID == "" || d.Host == "" {
			return errors.New("旧设备登记缺少 external_id/host；源未改动，可修复后重试")
		}
		tok, err := hex.DecodeString(d.TokenHex)
		if err != nil || len(tok) != 16 {
			return fmt.Errorf("设备 %s token 形状无效；源未改动", d.ExternalID)
		}
		avail := true
		// 能力以 Spec/Map 投影为准；旧声明保留（合并），投影失败不阻断迁移。
		caps := d.Capabilities
		if projected, perr := projectCapabilities(ctx, d.Model); perr == nil {
			caps = mergeCapabilities(d.Capabilities, projected)
		} else {
			ctx.Log(2, "hx-mi capability projection fallback: "+perr.Error())
		}
		if err = ctx.DeviceAnnounce(plugin.DeviceAnnounceRequest{ExternalID: d.ExternalID, Name: d.Name, Vendor: "xiaomi", Model: d.Model, Capabilities: caps, Available: &avail}); err != nil {
			return fmt.Errorf("设备 %s announce 失败，未提交: %w", d.ExternalID, err)
		}
		info, err := ctx.LegacyDeviceSecretSeal("hx_mi_legacy", "", d.ExternalID, "miio_token", tok)
		if err != nil {
			return fmt.Errorf("设备 %s token 密封失败，未提交: %w", d.ExternalID, err)
		}
		check, err := ctx.LegacyDeviceSecretInfo("hx_mi_legacy", "", d.ExternalID, "miio_token")
		if err != nil {
			return fmt.Errorf("设备 %s token 元信息校验失败，未提交: %w", d.ExternalID, err)
		}
		want := fmt.Sprintf("%x", sha256.Sum256(tok))
		if check.Version != info.Version || check.SHA256 != want {
			return fmt.Errorf("设备 %s token Vault 摘要不一致，未提交", d.ExternalID)
		}
		next.Devices = append(next.Devices, miioDevice{ExternalID: d.ExternalID, Host: d.Host, TokenRef: "miio_token", Model: d.Model, Name: d.Name, Capabilities: caps})
	}
	clean, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	w := plugin.FSWriteRequest{Path: "devices.json", QuotaKB: 4096}
	w.SetData(clean)
	if err = ctx.FSWrite(w); err != nil {
		return fmt.Errorf("registry v2 写入失败，旧源未提交: %w", err)
	}
	r, err := ctx.FSRead("devices.json")
	if err != nil || !r.Found {
		return errors.New("registry v2 写后回读失败，旧源未提交")
	}
	rb, _ := r.Data()
	if sha256.Sum256(rb) != sha256.Sum256(clean) {
		return errors.New("registry v2 写后摘要不一致，旧源未提交")
	}
	return nil
}
