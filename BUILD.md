# HX-MI 编译说明

本插件是 HOMEX 平台的 WASM 插件，需要 HOMEX 平台 SDK 才能编译。

## 前置

```bash
git clone https://github.com/1169892406/HOMEX.git  # 平台仓库（含 SDK）
cd HOMEX/homex
```

## 编译

```bash
cd plugins/official/xiaomi-miot
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm .
```

## 打包签名

```bash
go run ./cmd/smartctl plugin package --dir plugins/official/xiaomi-miot --out /tmp/hxmi.hpkg
go run ./cmd/smartctl plugin sign /tmp/hxmi.hpkg --key-id <your-key> --key <your-key-path>
```

## 安装

直接下载 Release 里的 .hpkg → HOMEX 插件市场 → 上传安装
