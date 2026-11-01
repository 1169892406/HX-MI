package main

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
)

// MIoTInstance is one audited model/type tuple from the official MIoT Spec v2 catalog.
type MIoTInstance struct {
	Model   string `json:"model"`
	Type    string `json:"type"`
	Status  string `json:"status"`
	Version int    `json:"version"`
}

type miotInstanceSnapshot struct {
	Instances []MIoTInstance `json:"instances"`
}

// 目录以 gzip 形式嵌入：5.9MB JSON 会挤爆 wasm 单文件 16MiB 上限，也拖慢
// 传输/校验。命令路径不再解析它（见 model_lookup.go 的紧凑表），只有
// catalog_status/coverage 这类工具才会解压。
//
//go:embed miotcatalog/instances.snapshot.json.gz
var miotInstancesSnapshotGz []byte

//go:embed miotcatalog/metadata.json
var miotInstancesMetadata []byte

func loadMIoTInstanceSnapshot() ([]MIoTInstance, error) {
	zr, err := gzip.NewReader(bytes.NewReader(miotInstancesSnapshotGz))
	if err != nil {
		return nil, fmt.Errorf("解压内置 MIoT 型号目录失败: %w", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("读取内置 MIoT 型号目录失败: %w", err)
	}
	var doc miotInstanceSnapshot
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("解析内置 MIoT 型号目录失败: %w", err)
	}
	if len(doc.Instances) == 0 {
		return nil, fmt.Errorf("内置 MIoT 型号目录为空")
	}
	return doc.Instances, nil
}
