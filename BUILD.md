
# 构建规范（2026-11-20 血泪三连）

正确命令（唯一正确）：

    GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm .

- `GOOS=js` 会产 gojs imports（宿主不装配）→ `module[gojs] not instantiated`
- `-buildmode=default`（command）导出 `_start`，宿主按 reactor 调导出时
  runtime 未初始化 → `homex_abi()` 触发 `runtime.notInitialized` abort
- `-buildmode=c-shared` 导出 `_initialize`（reactor），与 159 及宿主 ABI 匹配
