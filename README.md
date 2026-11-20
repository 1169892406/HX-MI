# HX-MI —— 小米米家设备接入插件

> 第三方社区插件（发布者 `hx-community`，未签名包）。装一次全平台生效：所有家庭共享同一份安装、配置与密钥。

接入小米米家设备：多账号登录与 Token 自动续期（过期自动重登）、云端设备同步、局域网直连控制、设备健康巡检、红外遥控学习与下发。

## 功能

- **多账号管理**：支持添加/切换多个小米账号，脱敏展示，独立会话与同步计划。
- **Token 自动续期**：开启「自动续 token」后保存账号密码（保险库密封），小米 serviceToken 过期时自动完整重登——判定以真实云端探针为准（30 分钟缓存），不再按时间盲估。
- **云端同步**：定时拉取米家设备清单（含规格缓存与中文控件名消歧），平台定时任务驱动，失败连续 3 次触发站内/APP 通知。
- **局域网直连**：LAN 优先（可配 cloud 优先），云会话失效不影响已登记设备的本地控制；每 5 秒健康巡检并回写在线状态。
- **规格化控件**：MIoT spec 自动投影能力与控件（开关/亮度/温度/挡位/模式…），同名控件自动消歧。
- **红外遥控**：红外码库检索、学习、原始码下发；空调/电视等红外设备的命名控件。
- **多家庭共享**：平台级单行安装（0410 架构），一个家庭安装，全部家庭可用。

## 安装

1. **HXCS 社区商店**：打开第三方插件开关后，商店内一键安装/更新（未签名包依赖平台 `plugin.allow_unsigned: true`）。
2. **平台上传**：`POST /api/v1/plugins/homex.xiaomi_miot/update`（`package_b64`）热升级，无需重启平台。

## 构建

见 [BUILD.md](BUILD.md)。唯一正确命令：

```bash
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm .
```

- `GOOS=js` 会产出 gojs imports（宿主 wazero 只装配 wasi + host）→ `module[gojs] not instantiated`
- 缺 `-buildmode=c-shared` 会导出 `_start`（command 模式），宿主按 reactor 调导出 → `runtime.notInitialized`
- 宿主 ABI 要求导出 `_initialize` + `homex_abi/buffer/buffer_len/dispatch`

## 目录速览

| 文件/目录 | 说明 |
|---|---|
| `manifest.json` | 插件清单（身份/权限/工具/资源声明） |
| `ui/` | 插件配置页（账号、同步、健康、红外） |
| `miotcatalog/` | MIoT 规格目录缓存 |
| `IR-STATUS.md` | 红外码库与能力现状 |
| `tools/`、`legacy-fixtures/` | 开发辅助与迁移对照 |

## 信任与签名

- 本插件**不自称官方、不带官方签名**；信任来源为 `unsigned`（第三方）。
- 平台侧「官方」判定唯一来源是密码学证据（`trust_source == signed_official`），目录自述不作为判据。
