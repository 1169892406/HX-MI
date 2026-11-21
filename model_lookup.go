package main

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/binary"
	"io"
	"sync"
)

// miotModelTable 是工具 tools/genmiotidx 生成的紧凑查找表（key→type）。
//
// 为什么不在运行时解析 instances.snapshot.json：WASM 内解析 5.9MB / 48k 条
// JSON 会吃满 60s 执行预算，表现为 driver.command 超时（t234 实测）。
// 表按 key 升序，偏移表 + 定长记录支持 O(log n) 二分，不构建全量 map。
//
// 表**以 gzip 形式嵌入**（2026-11-21 改）：
//
//	未压缩 6.34MB + 其余代码 ≈ 20.6MB 的 plugin.wasm —— 直接撞上平台
//	「单文件解压后上限 16MiB」，安装/升级被拒：`PLUGIN_PACKAGE_INVALID`。
//	gzip 后 0.99MB，wasm 回到 ~15MB，留出余量。
//
// 代价是首次查表要解压一次（6.3MB，wasm 内约数百毫秒），用 sync.Once 摊到
// 整个实例生命周期；解压失败按"查不到型号"处理（调用方本来就要容忍这种情形：
// 型号不在目录 ⇒ 不投影控件），不会让插件起不来。
//
//go:embed miotcatalog/models.bin.gz
var miotModelTableGz []byte

var (
	miotModelTableOnce sync.Once
	miotModelTable     []byte
)

// modelTable 返回解压后的紧凑表（失败返回 nil，调用方按"无目录"处理）。
func modelTable() []byte {
	miotModelTableOnce.Do(func() {
		zr, err := gzip.NewReader(bytes.NewReader(miotModelTableGz))
		if err != nil {
			return
		}
		defer zr.Close()
		b, err := io.ReadAll(zr)
		if err != nil {
			return
		}
		miotModelTable = b
	})
	return miotModelTable
}

const modelTableMagic = "MXMI1\n"

func lookupModelType(model string) (string, bool) {
	t := modelTable()
	if len(t) < 10 || string(t[:6]) != modelTableMagic {
		return "", false
	}
	count := int(binary.BigEndian.Uint32(t[6:10]))
	offsStart := 10
	if count == 0 || len(t) < offsStart+count*4 {
		return "", false
	}
	blob := t[offsStart+count*4:]
	readCStr := func(off uint32) (string, bool) {
		if int(off) >= len(blob) {
			return "", false
		}
		end := bytes.IndexByte(blob[off:], 0)
		if end < 0 {
			return "", false
		}
		return string(blob[off : int(off)+end]), true
	}
	lo, hi := 0, count
	for lo < hi {
		mid := (lo + hi) / 2
		off := binary.BigEndian.Uint32(t[offsStart+mid*4:])
		key, ok := readCStr(off)
		if !ok {
			return "", false
		}
		cmp := bytes.Compare([]byte(model), []byte(key))
		if cmp == 0 {
			typeOff := int(off) + len(key) + 1
			typ, ok := readCStr(uint32(typeOff))
			if !ok || typ == "" {
				return "", false
			}
			return typ, true
		}
		if cmp < 0 {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return "", false
}
