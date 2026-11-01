package main

import (
	"encoding/base64"
	"encoding/json"
	"sync/atomic"
	"time"

	"homex.dev/homex/sdk/go/plugin"
)

// syncState 是同步状态的**持久化**快照，对应旧版 httpapi.SyncStatusView。
//
// # 为什么必须落盘，而不是每次现算
//
// 旧版配置页显示的是"这台机器上一次真正同步发生了什么"：最近成功时间、
// 下次自动同步时间、连续失败次数、最后错误、以及那一次同步到的设备清单。
// 这些**没有一件能现算** —— 现算只能回答"现在能不能连上云"，
// 回答不了"上次成功是几点"（用户排查"是不是一直没同步上"就靠这个）。
//
// 新版最初没落盘，于是 account 工具只能返回 state/relogin_required/device_count，
// 前端拿不到数据，同步卡只剩空壳。用户口径：「旧版没有高级选项，同步结果有列表
// 的等，新版的丢失了很多信息」—— 丢的就是这份快照。
const syncStatePath = "sync-state.json"

// syncInFlight 标记"此刻正有一次同步在跑"。原子量而非文件：它是进程内瞬时
// 事实，落盘反而会让崩溃后留下永真的 running=true。
var syncInFlight atomic.Bool

// syncDeviceView 是配置页设备表的一行（安全视图：无令牌、无 IP、无 MAC）。
// 字段与旧版 httpapi.SyncDeviceView 逐字对齐。
type syncDeviceView struct {
	DID          string `json:"did"`
	Name         string `json:"name"`
	Model        string `json:"model"`
	Controllable bool   `json:"controllable"`
	Available    bool   `json:"available"`
	// Online 是云端上报的实时在线状态；Available 是"云端是否仍存在"。
	// 两者必须分开：设备断网时 available=true、online=false。
	Online     bool   `json:"online"`
	Reason     string `json:"reason,omitempty"`
	ReasonText string `json:"reason_text,omitempty"`
}

// syncStatusView 是 account 工具返回的 sync 块，字段与旧版
// httpapi.SyncStatusView 逐条对应（含 json 名）。
type syncStatusView struct {
	Enabled      bool   `json:"enabled"`
	Running      bool   `json:"running"`
	Devices      int    `json:"devices"`
	Controllable int    `json:"controllable"`
	Uncontrollable int  `json:"uncontrollable"`
	DeviceList   []syncDeviceView `json:"device_list,omitempty"`
	LastOKAt     string `json:"last_ok_at,omitempty"`
	NextSyncAt   string `json:"next_sync_at,omitempty"`
	ConsecutiveFailures int `json:"consecutive_failures"`
	LastError    string `json:"last_error,omitempty"`
}

// syncStateRecord 是落盘结构：上次同步结果 + 失败计数。
type syncStateRecord struct {
	Version             int              `json:"version"`
	LastOKAt            string           `json:"last_ok_at,omitempty"`
	LastAttemptAt       string           `json:"last_attempt_at,omitempty"`
	ConsecutiveFailures int              `json:"consecutive_failures"`
	LastError           string           `json:"last_error,omitempty"`
	DeviceList          []syncDeviceView `json:"device_list,omitempty"`
	Devices             int              `json:"devices"`
	Controllable        int              `json:"controllable"`
	Uncontrollable      int              `json:"uncontrollable"`
}

func loadSyncState(ctx *plugin.Context) syncStateRecord {
	var out syncStateRecord
	r, err := ctx.FSRead(syncStatePath)
	if err != nil || !r.Found {
		return out
	}
	b, err := r.Data()
	if err != nil {
		return out
	}
	// 文件损坏不阻断：当作"从未同步过"，下一次同步会覆盖。
	if json.Unmarshal(b, &out) != nil {
		return syncStateRecord{}
	}
	return out
}

func saveSyncState(ctx *plugin.Context, st syncStateRecord) error {
	st.Version = 1
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return ctx.FSWrite(plugin.FSWriteRequest{
		Path:    syncStatePath,
		DataB64: base64.StdEncoding.EncodeToString(b),
	})
}

// recordSyncSuccess 在一次成功同步后落盘结果快照。
func recordSyncSuccess(ctx *plugin.Context, devices []syncDeviceView) {
	st := loadSyncState(ctx)
	now := time.Now().UTC().Format(time.RFC3339)
	st.LastOKAt = now
	st.LastAttemptAt = now
	st.ConsecutiveFailures = 0
	st.LastError = ""
	st.DeviceList = devices
	st.Devices = len(devices)
	controllable, uncontrollable := 0, 0
	for _, d := range devices {
		if d.Controllable {
			controllable++
		} else {
			uncontrollable++
		}
	}
	st.Controllable = controllable
	st.Uncontrollable = uncontrollable
	if err := saveSyncState(ctx, st); err != nil {
		ctx.Log(2, "hx-mi save sync-state failed: "+err.Error())
	}
}

// recordSyncFailure 记一次失败：连续失败 +1，错误原样留下（不吞、不美化）。
func recordSyncFailure(ctx *plugin.Context, cause error) {
	st := loadSyncState(ctx)
	st.LastAttemptAt = time.Now().UTC().Format(time.RFC3339)
	st.ConsecutiveFailures++
	if cause != nil {
		st.LastError = cause.Error()
	}
	if err := saveSyncState(ctx, st); err != nil {
		ctx.Log(2, "hx-mi save sync-state failed: "+err.Error())
	}
}

// buildAccountView 组装 account 工具的完整应答。
//
// # 为什么要回到旧版的字段集
//
// 新版第一版只返回 state/relogin_required/device_count，配置页因此剩下空壳：
// 没有账号名、没有区域、没有同步时间、没有设备列表（用户：「新版的丢失了很多
// 信息」）。这里按旧版 `/v1/account` 的形状补齐：identity 三件（account /
// region / saved_at）+ 整块 sync。
//
// sync.enabled / running / next_sync_at 现算（读设置与状态文件即可）；
// 其余字段来自上一次同步落盘的快照（sync-state.json），因为没有一件能现推。
func buildAccountView(ctx *plugin.Context) (json.RawMessage, error) {
	state, err := restoreCloudSession(ctx)
	if err != nil {
		return nil, err
	}
	ds, _ := loadDeviceRegistry(ctx)
	st := loadSyncState(ctx)
	settings, _ := loadSyncSettings(ctx)

	// 账号身份：从已保存的会话材料里取（脱敏后展示）。
	out := map[string]any{
		"state":           state,
		"relogin_required": state == "auth_rejected",
		"device_count":    len(ds),
	}
	if id := loadAccountIdentity(ctx); id != nil {
		if id.Account != "" {
			out["account"] = id.Account
		}
		if id.Masked != "" {
			out["masked_user"] = id.Masked
		}
		if id.Region != "" {
			out["region"] = id.Region
		}
		if id.SavedAt != "" {
			out["saved_at"] = id.SavedAt
		}
	}

	// next_sync_at 只在自动同步开启时才有意义：关闭态给空串，前端据此显示"已关闭"
	// （旧版同一口径：手动同步不改变这个时间）。
	next := ""
	if settings.Enabled && st.LastOKAt != "" {
		if t, perr := time.Parse(time.RFC3339, st.LastOKAt); perr == nil {
			next = t.Add(time.Duration(settings.IntervalSeconds) * time.Second).Format(time.RFC3339)
		}
	}

	out["sync"] = syncStatusView{
		Enabled:             settings.Enabled,
		Running:             syncInFlight.Load(),
		Devices:             st.Devices,
		Controllable:        st.Controllable,
		Uncontrollable:      st.Uncontrollable,
		DeviceList:          st.DeviceList,
		LastOKAt:            st.LastOKAt,
		NextSyncAt:          next,
		ConsecutiveFailures: st.ConsecutiveFailures,
		LastError:           st.LastError,
	}
	return json.Marshal(out)
}

// accountIdentity 是会话里可用于展示的账号身份（**不含任何凭据**）。
type accountIdentity struct {
	Account string `json:"account,omitempty"`
	Masked  string `json:"masked_user,omitempty"`
	Region  string `json:"region,omitempty"`
	SavedAt string `json:"saved_at,omitempty"`
	// UserID 是小米数字账号 ID（step1 与 passToken 配合使用）。
	UserID string `json:"user_id,omitempty"`
}

// loadAccountIdentity 读 cloud-session.json 里的展示字段。
//
// 为什么单独读而不是从 openCloudSession 取：它只解出 ssecurity，
// 这里要的是给人看的三个字段，且**绝不能**把 ssecurity 带出来。
func loadAccountIdentity(ctx *plugin.Context) *accountIdentity {
	r, err := ctx.FSRead("cloud-session.json")
	if err != nil || !r.Found {
		return nil
	}
	b, err := r.Data()
	if err != nil {
		return nil
	}
	var saved struct {
		Account    string `json:"account"`
		MaskedUser string `json:"masked_user"`
		Region     string `json:"region"`
		SavedAt    string `json:"saved_at"`
	}
	if json.Unmarshal(b, &saved) != nil {
		return nil
	}
	id := accountIdentity{
		Account: saved.Account, Masked: saved.MaskedUser,
		Region: saved.Region, SavedAt: saved.SavedAt,
	}
	// 完整账号也可能只在保险库里（旧会话落盘时还没有 account 字段）。
	// 取不到就保持空，让前端回落到 masked_user —— 绝不用脱敏值冒充完整值。
	if id.Account == "" {
		if sec, _, serr := ctx.VaultBlobOpen("xiaomi_session_material"); serr == nil {
			var mat cloudSecretMaterial
			if json.Unmarshal(sec, &mat) == nil {
				if mat.Account != "" {
					id.Account = mat.Account
				}
				if mat.UserID != "" {
					id.UserID = mat.UserID
				}
			}
		}
	}
	if id.Account == "" && id.Masked == "" && id.Region == "" && id.SavedAt == "" {
		return nil
	}
	return &id
}
