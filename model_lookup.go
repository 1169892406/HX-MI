package main

import (
	"bytes"
	"encoding/binary"
	_ "embed"
)

// miotModelTable 是工具 tools/genmiotidx 生成的紧凑查找表（key→type）。
//
// 为什么不在运行时解析 instances.snapshot.json：WASM 内解析 5.9MB / 48k 条
// JSON 会吃满 60s 执行预算，表现为 driver.command 超时（t234 实测）。
// 表按 key 升序，偏移表 + 定长记录支持 O(log n) 二分，不构建全量 map。
//
// 表**不压缩**：命令路径每次进程启动都要用，gzip 解压 6.3MB 在 -race 下的
// 宿主管道里会把 60s 预算吃光（t205 实测）。未压缩后 WASM 仍 <16MiB。
//
//go:embed miotcatalog/models.bin
var miotModelTable []byte

const modelTableMagic = "MXMI1\n"

func lookupModelType(model string) (string, bool) {
	t := miotModelTable
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
