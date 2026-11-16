package main

import (
	"encoding/json"
	"errors"
	"strings"

	"homex.dev/homex/sdk/go/plugin"
)

// hxSyncSettings 是前端“同步设置”卡片的持久化视图。
type hxSyncSettings struct {
	IntervalSeconds int    `json:"interval_seconds"`
	Enabled         bool   `json:"enabled"`
	HouseholdID     string `json:"household_id"`
}

const hxSyncSettingsPath = "sync-settings.json"

func defaultSyncSettings(hh string) hxSyncSettings {
	return hxSyncSettings{IntervalSeconds: current.SyncIntervalSec, Enabled: true, HouseholdID: hh}
}

func loadSyncSettings(ctx *plugin.Context) (hxSyncSettings, error) {
	out := defaultSyncSettings(ctx.HouseholdID)
	r, err := ctx.FSRead(hxSyncSettingsPath)
	if err != nil || !r.Found {
		return out, nil
	}
	b, err := r.Data()
	if err != nil {
		return out, err
	}
	// 文件损坏不阻断：回落默认值，下一次保存会覆盖。
	if json.Unmarshal(b, &out) != nil {
		return defaultSyncSettings(ctx.HouseholdID), nil
	}
	if out.IntervalSeconds < 60 || out.IntervalSeconds > 86400 {
		out.IntervalSeconds = current.SyncIntervalSec
	}
	return out, nil
}

func saveSyncSettings(ctx *plugin.Context, s hxSyncSettings) error {
	b, _ := json.Marshal(s)
	w := plugin.FSWriteRequest{Path: hxSyncSettingsPath, QuotaKB: 64}
	w.SetData(b)
	return ctx.FSWrite(w)
}

// applySyncSettingsTool 处理前端 settings 的读写。
//
// 只接受三个可写字段；未知字段一律拒绝（前端拼写错误不该静默生效）。
func applySyncSettingsTool(ctx *plugin.Context, args json.RawMessage) (json.RawMessage, error) {
	s, err := loadSyncSettings(ctx)
	if err != nil {
		return nil, err
	}
	var in struct {
		IntervalSeconds *int    `json:"interval_seconds"`
		Enabled         *bool   `json:"enabled"`
		HouseholdID     *string `json:"household_id"`
	}
	if len(strings.TrimSpace(string(args))) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, err
		}
	}
	changed := false
	if in.IntervalSeconds != nil {
		if *in.IntervalSeconds < 60 || *in.IntervalSeconds > 86400 {
			return nil, errors.New("interval_seconds 必须在 60 到 86400 之间")
		}
		s.IntervalSeconds = *in.IntervalSeconds
		changed = true
	}
	if in.Enabled != nil {
		s.Enabled = *in.Enabled
		changed = true
	}
	if changed {
		if err := saveSyncSettings(ctx, s); err != nil {
			return nil, err
		}
		current.SyncIntervalSec = s.IntervalSeconds
		// 保存即生效：同名 background 注册由宿主替换间隔。
		interval := s.IntervalSeconds * 1000
		if err := ctx.BackgroundRegister(plugin.BackgroundRegisterRequest{
			Name: "account-sync", IntervalMS: interval, Immediate: false,
		}); err != nil {
			return nil, err
		}
		/*
		 * ★ 2026-11-16：间隔改了，**平台那条真正干活的定时任务也必须跟着改**。
		 * 只改 background tick 的话，用户把 30 分钟改成一小时，界面上会显示
		 * 一小时，而实际推送仍是旧频率 —— 又一处"显示与行为不一致"。
		 * 同名 upsert，不会产生重复任务（见 registerAutoSyncSchedule）。
		 * 失败只 Warn：不能用"定时任务注册不上"把用户保存设置的动作也挡掉。
		 */
		if err := registerAutoSyncSchedule(ctx, s.IntervalSeconds); err != nil {
			ctx.LogWarn("自动同步定时任务更新失败（手动同步不受影响）：%v", err)
		}
	}
	return json.Marshal(map[string]any{
		"interval_seconds":   s.IntervalSeconds,
		"enabled":            s.Enabled,
		"household_id":       ctx.HouseholdID,
		"household_editable": false,
	})
}
