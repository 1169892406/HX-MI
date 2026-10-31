package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	"homex.dev/homex/sdk/go/plugin"
)

// migrateLegacy 在 Tool/Configure 的宿主调用栈内执行，使用宿主一致快照；只有全部
// 文件写入新命名空间并能回读后才 commit。commit 失败保持旧源可恢复。
func migrateLegacy(ctx *plugin.Context) error {
	status, err := ctx.LegacyStatus("hx_mi_legacy")
	if err != nil {
		return err
	}
	if status.Migrated {
		return nil
	}
	snap, err := ctx.LegacySnapshot("hx_mi_legacy")
	if err != nil {
		return err
	}
	files, err := ctx.LegacyList(snap.SnapshotID)
	if err != nil {
		return err
	}
	allowed := map[string]bool{"session.enc": true, "accounts.json": true, "credentials.vault": true, "device_id": true, "devices.json": true, "ownership.json": true}
	var legacyDevices []byte
	for _, f := range files {
		clean := path.Clean(f.Path)
		if clean != f.Path || strings.Contains(clean, "..") || !allowed[clean] {
			continue
		}
		b, err := ctx.LegacyRead(snap.SnapshotID, clean, f.Size+1)
		if err != nil {
			return fmt.Errorf("读取旧数据 %s: %w", clean, err)
		}
		if int64(len(b)) != f.Size {
			return fmt.Errorf("旧数据 %s 长度不符：清单 %d，实际 %d；源未提交，可修复后重试", clean, f.Size, len(b))
		}
		gotHash := sha256.Sum256(b)
		if !digestMatches(f.SHA256, gotHash[:]) {
			return fmt.Errorf("旧数据 %s SHA256 与快照清单不符；源未提交，可重新快照重试", clean)
		}
		if clean == "devices.json" {
			legacyDevices = append([]byte(nil), b...)
			continue
		}
		w := plugin.FSWriteRequest{Path: "legacy/" + clean, QuotaKB: 4096}
		w.SetData(b)
		if err = ctx.FSWrite(w); err != nil {
			return err
		}
		back, err := ctx.FSRead("legacy/" + clean)
		if err != nil || !back.Found {
			return fmt.Errorf("旧数据 %s 写入后回读失败；未提交，可覆盖暂存后重试", clean)
		}
		bb, err := back.Data()
		if err != nil {
			return fmt.Errorf("旧数据 %s 回读解码失败: %w", clean, err)
		}
		backHash := sha256.Sum256(bb)
		if !bytes.Equal(bb, b) || backHash != gotHash {
			return fmt.Errorf("旧数据 %s 写后内容或 SHA256 不一致；未提交，可覆盖暂存后重试", clean)
		}
	}
	// devices.json 是特殊敏感数据：必须在同一 snapshot 事务中完成 announce、
	// device Blob seal/open 和无 token registry v2 回读，成功后才允许 commit。
	if len(legacyDevices) > 0 {
		if err := migrateDeviceTokens(ctx, legacyDevices); err != nil {
			return fmt.Errorf("设备 Token 迁移失败，Legacy 未提交可重试: %w", err)
		}
	}
	return ctx.LegacyCommit(snap.SnapshotID)
}

// LegacyImport 在失败时不 commit，legacy/* 暂存允许下一轮 FSWrite 原子覆盖；
// 旧源由宿主快照管理且保持不动。摘要兼容宿主可能返回的 hex 或 base64。
func digestMatches(want string, got []byte) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return false
	}
	if b, e := hex.DecodeString(want); e == nil {
		return bytes.Equal(b, got)
	}
	if b, e := base64.StdEncoding.DecodeString(want); e == nil {
		return bytes.Equal(b, got)
	}
	return false
}
